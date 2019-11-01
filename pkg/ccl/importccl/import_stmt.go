// Copyright 2017 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package importccl

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/ccl/backupccl"
	"github.com/cockroachdb/cockroach/pkg/internal/client"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/server/telemetry"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/privilege"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/sql/stats"
	"github.com/cockroachdb/cockroach/pkg/storage/cloud"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/errorutil/unimplemented"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/humanizeutil"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/retry"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/cockroachdb/errors"
)

const (
	csvDelimiter    = "delimiter"
	csvComment      = "comment"
	csvNullIf       = "nullif"
	csvSkip         = "skip"
	csvStrictQuotes = "strict_quotes"

	mysqlOutfileRowSep   = "rows_terminated_by"
	mysqlOutfileFieldSep = "fields_terminated_by"
	mysqlOutfileEnclose  = "fields_enclosed_by"
	mysqlOutfileEscape   = "fields_escaped_by"

	importOptionSSTSize          = "sstsize"
	importOptionDecompress       = "decompress"
	importOptionOversample       = "oversample"
	importOptionSkipFKs          = "skip_foreign_keys"
	importOptionDisableGlobMatch = "disable_glob_matching"
	importOptionSaveRejected     = "experimental_save_rejected"

	pgCopyDelimiter = "delimiter"
	pgCopyNull      = "nullif"

	pgMaxRowSize = "max_row_size"
)

var importOptionExpectValues = map[string]sql.KVStringOptValidate{
	csvDelimiter:    sql.KVStringOptRequireValue,
	csvComment:      sql.KVStringOptRequireValue,
	csvNullIf:       sql.KVStringOptRequireValue,
	csvSkip:         sql.KVStringOptRequireValue,
	csvStrictQuotes: sql.KVStringOptRequireNoValue,

	mysqlOutfileRowSep:   sql.KVStringOptRequireValue,
	mysqlOutfileFieldSep: sql.KVStringOptRequireValue,
	mysqlOutfileEnclose:  sql.KVStringOptRequireValue,
	mysqlOutfileEscape:   sql.KVStringOptRequireValue,

	importOptionSSTSize:      sql.KVStringOptRequireValue,
	importOptionDecompress:   sql.KVStringOptRequireValue,
	importOptionOversample:   sql.KVStringOptRequireValue,
	importOptionSaveRejected: sql.KVStringOptRequireNoValue,

	importOptionSkipFKs:          sql.KVStringOptRequireNoValue,
	importOptionDisableGlobMatch: sql.KVStringOptRequireNoValue,

	pgMaxRowSize: sql.KVStringOptRequireValue,
}

func importJobDescription(
	p sql.PlanHookState,
	orig *tree.Import,
	defs tree.TableDefs,
	files []string,
	opts map[string]string,
) (string, error) {
	stmt := *orig
	stmt.CreateFile = nil
	stmt.CreateDefs = defs
	stmt.Files = nil
	for _, file := range files {
		clean, err := cloud.SanitizeExternalStorageURI(file)
		if err != nil {
			return "", err
		}
		stmt.Files = append(stmt.Files, tree.NewDString(clean))
	}
	stmt.Options = nil
	for k, v := range opts {
		opt := tree.KVOption{Key: tree.Name(k)}
		val := importOptionExpectValues[k] == sql.KVStringOptRequireValue
		val = val || (importOptionExpectValues[k] == sql.KVStringOptAny && len(v) > 0)
		if val {
			opt.Value = tree.NewDString(v)
		}
		stmt.Options = append(stmt.Options, opt)
	}
	sort.Slice(stmt.Options, func(i, j int) bool { return stmt.Options[i].Key < stmt.Options[j].Key })
	ann := p.ExtendedEvalContext().Annotations
	return tree.AsStringWithFQNames(&stmt, ann), nil
}

// importPlanHook implements sql.PlanHookFn.
func importPlanHook(
	_ context.Context, stmt tree.Statement, p sql.PlanHookState,
) (sql.PlanHookRowFn, sqlbase.ResultColumns, []sql.PlanNode, bool, error) {
	importStmt, ok := stmt.(*tree.Import)
	if !ok {
		return nil, nil, nil, false, nil
	}

	if !p.ExecCfg().Settings.Version.IsActive(cluster.VersionPartitionedBackup) {
		return nil, nil, nil, false, errors.Errorf("IMPORT requires a cluster fully upgraded to version >= 19.2")
	}

	filesFn, err := p.TypeAsStringArray(importStmt.Files, "IMPORT")
	if err != nil {
		return nil, nil, nil, false, err
	}

	var createFileFn func() (string, error)
	if !importStmt.Bundle && !importStmt.Into && importStmt.CreateDefs == nil {
		createFileFn, err = p.TypeAsString(importStmt.CreateFile, "IMPORT")
		if err != nil {
			return nil, nil, nil, false, err
		}
	}

	optsFn, err := p.TypeAsStringOpts(importStmt.Options, importOptionExpectValues)
	if err != nil {
		return nil, nil, nil, false, err
	}

	fn := func(ctx context.Context, _ []sql.PlanNode, resultsCh chan<- tree.Datums) error {
		// TODO(dan): Move this span into sql.
		ctx, span := tracing.ChildSpan(ctx, importStmt.StatementTag())
		defer tracing.FinishSpan(span)

		walltime := p.ExecCfg().Clock.Now().WallTime

		if err := p.RequireAdminRole(ctx, "IMPORT"); err != nil {
			return err
		}

		if !p.ExtendedEvalContext().TxnImplicit {
			return errors.Errorf("IMPORT cannot be used inside a transaction")
		}

		opts, err := optsFn()
		if err != nil {
			return err
		}

		filenamePatterns, err := filesFn()
		if err != nil {
			return err
		}
		var files []string
		if _, ok := opts[importOptionDisableGlobMatch]; ok {
			files = filenamePatterns
		} else {
			for _, file := range filenamePatterns {
				if cloud.URINeedsGlobExpansion(file) {
					s, err := cloud.ExternalStorageFromURI(ctx, file, p.ExecCfg().Settings)
					if err != nil {
						return err
					}
					expandedFiles, err := s.ListFiles(ctx)
					if err != nil {
						return err
					}
					if len(expandedFiles) < 1 {
						return errors.Errorf(`no files matched uri provided: '%s'`, file)
					}
					files = append(files, expandedFiles...)
				} else {
					files = append(files, file)
				}
			}
		}

		table := importStmt.Table

		var parentID sqlbase.ID
		if table != nil {
			// We have a target table, so it might specify a DB in its name.
			found, descI, err := table.ResolveTarget(ctx,
				p, p.SessionData().Database, p.SessionData().SearchPath)
			if err != nil {
				return pgerror.Wrap(err, pgcode.UndefinedTable,
					"resolving target import name")
			}
			if !found {
				// Check if database exists right now. It might not after the import is done,
				// but it's better to fail fast than wait until restore.
				return pgerror.Newf(pgcode.UndefinedObject,
					"database does not exist: %q", table)
			}
			parentID = descI.(*sqlbase.DatabaseDescriptor).ID
		} else {
			// No target table means we're importing whatever we find into the session
			// database, so it must exist.
			dbDesc, err := p.ResolveUncachedDatabaseByName(ctx, p.SessionData().Database, true /*required*/)
			if err != nil {
				return pgerror.Wrap(err, pgcode.UndefinedObject,
					"could not resolve current database")
			}
			parentID = dbDesc.ID
		}

		format := roachpb.IOFileFormat{}
		switch importStmt.FileFormat {
		case "CSV":
			telemetry.Count("import.format.csv")
			format.Format = roachpb.IOFileFormat_CSV
			if override, ok := opts[csvDelimiter]; ok {
				comma, err := util.GetSingleRune(override)
				if err != nil {
					return pgerror.Wrap(err, pgcode.Syntax, "invalid comma value")
				}
				format.Csv.Comma = comma
			}

			if override, ok := opts[csvComment]; ok {
				comment, err := util.GetSingleRune(override)
				if err != nil {
					return pgerror.Wrap(err, pgcode.Syntax, "invalid comment value")
				}
				format.Csv.Comment = comment
			}

			if override, ok := opts[csvNullIf]; ok {
				format.Csv.NullEncoding = &override
			}

			if override, ok := opts[csvSkip]; ok {
				skip, err := strconv.Atoi(override)
				if err != nil {
					return pgerror.Wrapf(err, pgcode.Syntax, "invalid %s value", csvSkip)
				}
				if skip < 0 {
					return pgerror.Newf(pgcode.Syntax, "%s must be >= 0", csvSkip)
				}
				format.Csv.Skip = uint32(skip)
			}
			if _, ok := opts[csvStrictQuotes]; ok {
				format.Csv.StrictQuotes = true
			}
		case "DELIMITED":
			telemetry.Count("import.format.mysqlout")
			format.Format = roachpb.IOFileFormat_MysqlOutfile
			format.MysqlOut = roachpb.MySQLOutfileOptions{
				RowSeparator:   '\n',
				FieldSeparator: '\t',
			}
			if override, ok := opts[mysqlOutfileRowSep]; ok {
				c, err := util.GetSingleRune(override)
				if err != nil {
					return pgerror.Wrapf(err, pgcode.Syntax,
						"invalid %q value", mysqlOutfileRowSep)
				}
				format.MysqlOut.RowSeparator = c
			}

			if override, ok := opts[mysqlOutfileFieldSep]; ok {
				c, err := util.GetSingleRune(override)
				if err != nil {
					return pgerror.Wrapf(err, pgcode.Syntax, "invalid %q value", mysqlOutfileFieldSep)
				}
				format.MysqlOut.FieldSeparator = c
			}

			if override, ok := opts[mysqlOutfileEnclose]; ok {
				c, err := util.GetSingleRune(override)
				if err != nil {
					return pgerror.Wrapf(err, pgcode.Syntax, "invalid %q value", mysqlOutfileRowSep)
				}
				format.MysqlOut.Enclose = roachpb.MySQLOutfileOptions_Always
				format.MysqlOut.Encloser = c
			}

			if override, ok := opts[mysqlOutfileEscape]; ok {
				c, err := util.GetSingleRune(override)
				if err != nil {
					return pgerror.Wrapf(err, pgcode.Syntax, "invalid %q value", mysqlOutfileRowSep)
				}
				format.MysqlOut.HasEscape = true
				format.MysqlOut.Escape = c
			}
			if override, ok := opts[csvSkip]; ok {
				skip, err := strconv.Atoi(override)
				if err != nil {
					return pgerror.Wrapf(err, pgcode.Syntax, "invalid %s value", csvSkip)
				}
				if skip < 0 {
					return pgerror.Newf(pgcode.Syntax, "%s must be >= 0", csvSkip)
				}
				format.MysqlOut.Skip = uint32(skip)
			}
			if override, ok := opts[csvNullIf]; ok {
				format.MysqlOut.NullEncoding = &override
			}
			// TODO(spaskob): Refactor so that the save rejected option
			// is passed in all import formats not just DELIMITED.
			if _, ok := opts[importOptionSaveRejected]; ok {
				format.MysqlOut.SaveRejected = true
			}
		case "MYSQLDUMP":
			telemetry.Count("import.format.mysqldump")
			format.Format = roachpb.IOFileFormat_Mysqldump
		case "PGCOPY":
			telemetry.Count("import.format.pgcopy")
			format.Format = roachpb.IOFileFormat_PgCopy
			format.PgCopy = roachpb.PgCopyOptions{
				Delimiter: '\t',
				Null:      `\N`,
			}
			if override, ok := opts[pgCopyDelimiter]; ok {
				c, err := util.GetSingleRune(override)
				if err != nil {
					return pgerror.Wrapf(err, pgcode.Syntax, "invalid %q value", pgCopyDelimiter)
				}
				format.PgCopy.Delimiter = c
			}
			if override, ok := opts[pgCopyNull]; ok {
				format.PgCopy.Null = override
			}
			maxRowSize := int32(defaultScanBuffer)
			if override, ok := opts[pgMaxRowSize]; ok {
				sz, err := humanizeutil.ParseBytes(override)
				if err != nil {
					return err
				}
				if sz < 1 || sz > math.MaxInt32 {
					return errors.Errorf("%s out of range: %d", pgMaxRowSize, sz)
				}
				maxRowSize = int32(sz)
			}
			format.PgCopy.MaxRowSize = maxRowSize
		case "PGDUMP":
			telemetry.Count("import.format.pgdump")
			format.Format = roachpb.IOFileFormat_PgDump
			maxRowSize := int32(defaultScanBuffer)
			if override, ok := opts[pgMaxRowSize]; ok {
				sz, err := humanizeutil.ParseBytes(override)
				if err != nil {
					return err
				}
				if sz < 1 || sz > math.MaxInt32 {
					return errors.Errorf("%s out of range: %d", pgMaxRowSize, sz)
				}
				maxRowSize = int32(sz)
			}
			format.PgDump.MaxRowSize = maxRowSize
		default:
			return unimplemented.Newf("import.format", "unsupported import format: %q", importStmt.FileFormat)
		}

		// sstSize, if 0, will be set to an appropriate default by the specific
		// implementation (local or distributed) since each has different optimal
		// settings.
		var sstSize int64
		if override, ok := opts[importOptionSSTSize]; ok {
			sz, err := humanizeutil.ParseBytes(override)
			if err != nil {
				return err
			}
			sstSize = sz
		}
		var oversample int64
		if override, ok := opts[importOptionOversample]; ok {
			os, err := strconv.ParseInt(override, 10, 64)
			if err != nil {
				return err
			}
			oversample = os
		}

		var skipFKs bool
		if _, ok := opts[importOptionSkipFKs]; ok {
			skipFKs = true
		}

		if override, ok := opts[importOptionDecompress]; ok {
			found := false
			for name, value := range roachpb.IOFileFormat_Compression_value {
				if strings.EqualFold(name, override) {
					format.Compression = roachpb.IOFileFormat_Compression(value)
					found = true
					break
				}
			}
			if !found {
				return unimplemented.Newf("import.compression", "unsupported compression value: %q", override)
			}
		}

		var tableDetails []jobspb.ImportDetails_Table
		jobDesc, err := importJobDescription(p, importStmt, nil, filenamePatterns, opts)
		if err != nil {
			return err
		}

		if importStmt.Into {
			// TODO(dt): this is a prototype for incremental import but there are many
			// TODOs remaining before it is ready to graduate to prime-time. Some of
			// them are captured in specific TODOs below, but some of the big, scary
			// things to do are:
			// - review planner vs txn use very carefully. We should try to get to a
			//   single txn used to plan the job and create it. Using the planner's
			//   txn today is very wrong since it will not commit until after the job
			//   has run, so starting a job based on reads it returned is very wrong.
			// - audit every place that we resolve/lease/read table descs to be sure
			//   that the IMPORTING state is handled correctly. SQL lease acquisition
			//   is probably the easy one here since it has single read path -- the
			//   things that read directly like the queues or background jobs are the
			//   ones we'll need to really carefully look though.
			// - Look at if/how cleanup/rollback works. Reconsider the cpu from the
			//   desc version (perhaps we should be re-reading instead?).
			// - Write _a lot_ of tests.
			found, err := p.ResolveMutableTableDescriptor(ctx, table, true, sql.ResolveRequireTableDesc)
			if err != nil {
				return err
			}

			// IMPORT INTO does not currently support interleaved tables.
			if found.IsInterleaved() {
				// TODO(miretskiy): Handle import into when tables are interleaved.
				return pgerror.New(pgcode.FeatureNotSupported, "Cannot use IMPORT INTO with interleaved tables")
			}

			// Validate target columns.
			var intoCols []string
			var isTargetCol = make(map[string]bool)
			for _, name := range importStmt.IntoCols {
				var err error
				if _, err = found.FindActiveColumnByName(name.String()); err != nil {
					return errors.Wrap(err, "verifying target columns")
				}

				isTargetCol[name.String()] = true
				intoCols = append(intoCols, name.String())
			}

			// IMPORT INTO does not support columns with DEFAULT expressions. Ensure
			// that all non-target columns are nullable until we support DEFAULT
			// expressions.
			for _, col := range found.VisibleColumns() {
				if col.HasDefault() {
					return errors.Errorf("cannot IMPORT INTO a table with a DEFAULT expression for any of its columns")
				}

				if len(isTargetCol) != 0 && !isTargetCol[col.Name] && !col.IsNullable() {
					return errors.Errorf("all non-target columns in IMPORT INTO must be nullable")
				}
			}

			tableDetails = []jobspb.ImportDetails_Table{{Desc: &found.TableDescriptor, IsNew: false, TargetCols: intoCols}}
		} else {
			var tableDescs []*sqlbase.TableDescriptor
			seqVals := make(map[sqlbase.ID]int64)

			if importStmt.Bundle {
				store, err := p.ExecCfg().DistSQLSrv.ExternalStorageFromURI(ctx, files[0])
				if err != nil {
					return err
				}
				defer store.Close()

				raw, err := store.ReadFile(ctx, "")
				if err != nil {
					return err
				}
				defer raw.Close()
				reader, err := decompressingReader(raw, files[0], format.Compression)
				if err != nil {
					return err
				}
				defer reader.Close()

				var match string
				if table != nil {
					match = table.TableName.String()
				}

				fks := fkHandler{skip: skipFKs, allowed: true, resolver: make(fkResolver)}
				switch format.Format {
				case roachpb.IOFileFormat_Mysqldump:
					evalCtx := &p.ExtendedEvalContext().EvalContext
					tableDescs, err = readMysqlCreateTable(ctx, reader, evalCtx, defaultCSVTableID, parentID, match, fks, seqVals)
				case roachpb.IOFileFormat_PgDump:
					evalCtx := &p.ExtendedEvalContext().EvalContext
					tableDescs, err = readPostgresCreateTable(reader, evalCtx, p.ExecCfg().Settings, match, parentID, walltime, fks, int(format.PgDump.MaxRowSize))
				default:
					return errors.Errorf("non-bundle format %q does not support reading schemas", format.Format.String())
				}
				if err != nil {
					return err
				}
				if tableDescs == nil && table != nil {
					return errors.Errorf("table definition not found for %q", table.TableName.String())
				}
			} else {
				if table == nil {
					return errors.Errorf("non-bundle format %q should always have a table name", importStmt.FileFormat)
				}
				var create *tree.CreateTable
				if importStmt.CreateDefs != nil {
					create = &tree.CreateTable{
						Table: *importStmt.Table,
						Defs:  importStmt.CreateDefs,
					}
				} else {
					filename, err := createFileFn()
					if err != nil {
						return err
					}
					create, err = readCreateTableFromStore(ctx, filename, p.ExecCfg().DistSQLSrv.ExternalStorageFromURI)
					if err != nil {
						return err
					}

					if table.TableName != create.Table.TableName {
						return errors.Errorf(
							"importing table %s, but file specifies a schema for table %s",
							table.TableName, create.Table.TableName,
						)
					}
				}

				tbl, err := MakeSimpleTableDescriptor(
					ctx, p.ExecCfg().Settings, create, parentID, defaultCSVTableID, NoFKs, walltime)
				if err != nil {
					return err
				}
				tableDescs = []*sqlbase.TableDescriptor{tbl.TableDesc()}
				descStr, err := importJobDescription(p, importStmt, create.Defs, filenamePatterns, opts)
				if err != nil {
					return err
				}
				jobDesc = descStr
			}

			tableDetails = make([]jobspb.ImportDetails_Table, len(tableDescs))
			for i := range tableDescs {
				tableDetails[i] = jobspb.ImportDetails_Table{Desc: tableDescs[i], SeqVal: seqVals[tableDescs[i].ID], IsNew: true}
			}
		}

		telemetry.CountBucketed("import.files", int64(len(files)))

		_, errCh, err := p.ExecCfg().JobRegistry.StartJob(ctx, resultsCh, jobs.Record{
			Description: jobDesc,
			Username:    p.User(),
			Details: jobspb.ImportDetails{
				URIs:       files,
				Format:     format,
				ParentID:   parentID,
				Tables:     tableDetails,
				SSTSize:    sstSize,
				Oversample: oversample,
				SkipFKs:    skipFKs,
			},
			Progress: jobspb.ImportProgress{},
		})
		if err != nil {
			return err
		}
		return <-errCh
	}
	return fn, backupccl.RestoreHeader, nil, false, nil
}

type importResumer struct {
	job            *jobs.Job
	settings       *cluster.Settings
	res            roachpb.BulkOpSummary
	statsRefresher *stats.Refresher

	testingKnobs struct {
		afterImport func() error
	}
}

// Prepares descriptors for newly created tables being imported into.
func prepareNewTableDescsForIngestion(
	ctx context.Context,
	txn *client.Txn,
	p sql.PlanHookState,
	tables []jobspb.ImportDetails_Table,
	parentID sqlbase.ID,
) ([]*sqlbase.TableDescriptor, error) {
	var tableDescs []*sqlbase.TableDescriptor
	for _, i := range tables {
		if err := backupccl.CheckTableExists(ctx, txn, parentID, i.Desc.Name); err != nil {
			return nil, err
		}
		tableDescs = append(tableDescs, i.Desc)
	}

	// Verification steps have passed, generate a new table ID if we're
	// restoring. We do this last because we want to avoid calling
	// GenerateUniqueDescID if there's any kind of error above.
	// Reserving a table ID now means we can avoid the rekey work during restore.
	tableRewrites := make(backupccl.TableRewriteMap)
	seqVals := make(map[sqlbase.ID]int64, len(tables))
	for _, tableDesc := range tables {
		id, err := sql.GenerateUniqueDescID(ctx, p.ExecCfg().DB)
		if err != nil {
			return nil, err
		}
		tableRewrites[tableDesc.Desc.ID] = &jobspb.RestoreDetails_TableRewrite{
			TableID:  id,
			ParentID: parentID,
		}
		seqVals[id] = tableDesc.SeqVal
	}

	if err := backupccl.RewriteTableDescs(tableDescs, tableRewrites, ""); err != nil {
		return nil, err
	}

	for i := range tableDescs {
		tableDescs[i].State = sqlbase.TableDescriptor_OFFLINE
		tableDescs[i].OfflineReason = "importing"

	}

	var seqValKVs []roachpb.KeyValue
	for i := range tableDescs {
		if v, ok := seqVals[tableDescs[i].ID]; ok && v != 0 {
			key, val, err := sql.MakeSequenceKeyVal(tableDescs[i], v, false)
			if err != nil {
				return nil, err
			}
			kv := roachpb.KeyValue{Key: key}
			kv.Value.SetInt(val)
			seqValKVs = append(seqValKVs, kv)
		}
	}

	// Write the new TableDescriptors and flip the namespace entries over to
	// them. After this call, any queries on a table will be served by the newly
	// imported data.
	if err := backupccl.WriteTableDescs(ctx, txn, nil, tableDescs, p.User(), p.ExecCfg().Settings, seqValKVs); err != nil {
		return nil, errors.Wrapf(err, "creating tables")
	}

	return tableDescs, nil
}

// Prepares descriptors for existing tables being imported into.
func prepareExistingTableDescForIngestion(
	ctx context.Context, txn *client.Txn, desc *sqlbase.TableDescriptor, p sql.PlanHookState,
) (*sqlbase.TableDescriptor, error) {
	if len(desc.Mutations) > 0 {
		return nil, errors.Errorf("cannot IMPORT INTO a table with schema changes in progress -- try again later (pending mutation %s)", desc.Mutations[0].String())
	}

	if err := p.CheckPrivilege(ctx, desc, privilege.CREATE); err != nil {
		return nil, err
	}

	// TODO(dt): Ensure no other schema changes can start during ingest.
	importing := *desc
	importing.Version++
	// Take the table offline for import.
	// TODO(dt): audit everywhere we get table descs (leases or otherwise) to
	// ensure that filtering by state handles IMPORTING correctly.
	importing.State = sqlbase.TableDescriptor_OFFLINE
	importing.OfflineReason = "importing"
	// TODO(dt): de-validate all the FKs.

	if err := txn.SetSystemConfigTrigger(); err != nil {
		return nil, err
	}

	// Note that this CPut is safe with respect to mixed-version descriptor
	// upgrade and downgrade, because IMPORT does not operate in mixed-version
	// states.
	// TODO(jordan,lucy): remove this comment once 19.2 is released.
	existingDesc, err := sqlbase.ConditionalGetTableDescFromTxn(ctx, txn, desc)
	if err != nil {
		return nil, errors.Wrap(err, "another operation is currently operating on the table")
	}
	err = txn.CPut(ctx,
		sqlbase.MakeDescMetadataKey(desc.ID),
		sqlbase.WrapDescriptor(&importing),
		existingDesc)
	if err != nil {
		return nil, errors.Wrap(err, "another operation is currently operating on the table")
	}

	return &importing, nil
	// NB: we need to wait for the schema change to show up before it is safe
	// to ingest, but rather than do that here, we'll wait for this schema
	// change in the job's Resume hook, before running the ingest phase. That
	// will hopefully let it get a head start on propagating, plus the more we
	// do in the job, the more that has automatic cleanup on rollback.
}

// prepareTableDescsForIngestion prepares table descriptors for the ingestion
// step of import. The descriptors are in an IMPORTING state (offline) on
// successful completion of this method.
func (r *importResumer) prepareTableDescsForIngestion(
	ctx context.Context, p sql.PlanHookState, details jobspb.ImportDetails,
) error {
	err := p.ExecCfg().DB.Txn(ctx, func(ctx context.Context, txn *client.Txn) error {
		importDetails := details
		importDetails.Tables = make([]jobspb.ImportDetails_Table, len(details.Tables))

		newTableDescToIdx := make(map[*sqlbase.TableDescriptor]int, len(importDetails.Tables))
		var hasExistingTables bool
		var err error
		var newTableDescs []jobspb.ImportDetails_Table
		var desc *sqlbase.TableDescriptor
		for i, table := range details.Tables {
			if !table.IsNew {
				desc, err = prepareExistingTableDescForIngestion(ctx, txn, table.Desc, p)
				if err != nil {
					return err
				}
				importDetails.Tables[i] = jobspb.ImportDetails_Table{Desc: desc, Name: table.Name,
					SeqVal:     table.SeqVal,
					IsNew:      table.IsNew,
					TargetCols: table.TargetCols}

				hasExistingTables = true
			} else {
				newTableDescToIdx[table.Desc] = i
				newTableDescs = append(newTableDescs, table)
			}
		}

		// Prepare the table descriptors for newly created tables being imported
		// into.
		//
		// TODO(adityamaru): This is still unnecessarily complicated. If we can get
		// the new table desc preparation to work on a per desc basis, rather than
		// requiring all the newly created descriptors, then this can look like the
		// call to prepareExistingTableDescForIngestion. Currently, FK references
		// misbehave when I tried to write the desc one at a time.
		if len(newTableDescs) != 0 {
			res, err := prepareNewTableDescsForIngestion(ctx, txn, p, newTableDescs, importDetails.ParentID)
			if err != nil {
				return err
			}
			for i, table := range res {
				importDetails.Tables[i] = jobspb.ImportDetails_Table{Desc: table,
					Name:       details.Tables[i].Name,
					SeqVal:     details.Tables[i].SeqVal,
					IsNew:      details.Tables[i].IsNew,
					TargetCols: details.Tables[i].TargetCols}
			}
		}

		importDetails.PrepareComplete = true

		// If we do not have pending schema changes on existing descriptors we can
		// choose our Walltime (to IMPORT from) immediately. Otherwise, we have to
		// wait for all nodes to see the same descriptor version before doing so.
		if !hasExistingTables {
			importDetails.Walltime = p.ExecCfg().Clock.Now().WallTime
		} else {
			importDetails.Walltime = 0
		}

		// Update the job once all descs have been prepared for ingestion.
		err = r.job.WithTxn(txn).SetDetails(ctx, importDetails)

		return err
	})
	return err
}

// Resume is part of the jobs.Resumer interface.
func (r *importResumer) Resume(
	ctx context.Context, phs interface{}, resultsCh chan<- tree.Datums,
) error {
	details := r.job.Details().(jobspb.ImportDetails)
	p := phs.(sql.PlanHookState)

	tables := make(map[string]*execinfrapb.ReadImportDataSpec_ImportTable, len(details.Tables))
	if details.Tables != nil {
		// Skip prepare stage on job resumption, if it has already been completed.
		if !details.PrepareComplete {
			if err := r.prepareTableDescsForIngestion(ctx, p, details); err != nil {
				return err
			}

			// Re-initialize details after prepare step.
			details = r.job.Details().(jobspb.ImportDetails)
		}

		for _, i := range details.Tables {
			if i.Name != "" {
				tables[i.Name] = &execinfrapb.ReadImportDataSpec_ImportTable{Desc: i.Desc, TargetCols: i.TargetCols}
			} else if i.Desc != nil {
				tables[i.Desc.Name] = &execinfrapb.ReadImportDataSpec_ImportTable{Desc: i.Desc, TargetCols: i.TargetCols}
			} else {
				return errors.Errorf("invalid table specification")
			}
		}
	}

	// In the case of importing into existing tables we must wait for all nodes
	// to see the same version of the updated table descriptor, after which we
	// shall chose a ts to import from.
	if details.Walltime == 0 {
		// TODO(dt): update job status to mention waiting for tables to go offline.
		for _, i := range details.Tables {
			if _, err := p.ExecCfg().LeaseManager.WaitForOneVersion(ctx, i.Desc.ID, retry.Options{}); err != nil {
				return err
			}
		}

		details.Walltime = p.ExecCfg().Clock.Now().WallTime
		if err := r.job.WithTxn(nil).SetDetails(ctx, details); err != nil {
			return err
		}
	}

	walltime := details.Walltime
	files := details.URIs
	format := details.Format

	res, err := sql.DistIngest(ctx, p, r.job, tables, files, format, walltime)
	if err != nil {
		return err
	}
	if r.testingKnobs.afterImport != nil {
		if err := r.testingKnobs.afterImport(); err != nil {
			return err
		}
	}

	r.res = res
	r.statsRefresher = p.ExecCfg().StatsRefresher
	return nil
}

// OnFailOrCancel is part of the jobs.Resumer interface. Removes data that has
// been committed from a import that has failed or been canceled. It does this
// by adding the table descriptors in DROP state, which causes the schema change
// stuff to delete the keys in the background.
func (r *importResumer) OnFailOrCancel(ctx context.Context, txn *client.Txn) error {
	details := r.job.Details().(jobspb.ImportDetails)

	// Needed to trigger the schema change manager.
	if err := txn.SetSystemConfigTrigger(); err != nil {
		return err
	}

	// If the prepare step of the import job was not completed then the
	// descriptors do not need to be rolled back as the txn updating them never
	// completed.
	if !details.PrepareComplete {
		return nil
	}

	var revert []*sqlbase.TableDescriptor
	for _, tbl := range details.Tables {
		if !tbl.IsNew {
			revert = append(revert, tbl.Desc)
		}
	}

	// NB: if a revert fails it will abort the rest of this failure txn, which is
	// also what brings tables back online. We _could_ change the error handling
	// or just move the revert into Resume()'s error return path, however it isn't
	// clear that just bringing a table back online with partially imported data
	// that may or may not be partially reverted is actually a good idea. It seems
	// better to do the revert here so that the table comes back if and only if,
	// it was rolled back to its pre-IMPORT state, and instead provide a manual
	// admin knob (e.g. ALTER TABLE REVERT TO SYSTEM TIME) if anything goes wrong.
	if len(revert) > 0 {
		// Sanity check Walltime so it doesn't become a TRUNCATE if there's a bug.
		if details.Walltime == 0 {
			return errors.Errorf("invalid pre-IMPORT time to rollback")
		}
		ts := hlc.Timestamp{WallTime: details.Walltime}.Prev()
		if err := sql.RevertTables(ctx, txn.DB(), revert, ts, sql.RevertTableDefaultBatchSize); err != nil {
			return errors.Wrap(err, "rolling back partially completed IMPORT")
		}
	}

	b := txn.NewBatch()
	for _, tbl := range details.Tables {
		tableDesc := *tbl.Desc
		tableDesc.Version++
		if tbl.IsNew {
			tableDesc.State = sqlbase.TableDescriptor_DROP
			// If the DropTime if set, a table uses RangeClear for fast data removal. This
			// operation starts at DropTime + the GC TTL. If we used now() here, it would
			// not clean up data until the TTL from the time of the error. Instead, use 1
			// (that is, 1ns past the epoch) to allow this to be cleaned up as soon as
			// possible. This is safe since the table data was never visible to users,
			// and so we don't need to preserve MVCC semantics.
			tableDesc.DropTime = 1
			var existingIDVal roachpb.Value
			existingIDVal.SetInt(int64(tableDesc.ID))
			tKey := sqlbase.NewTableKey(tableDesc.ParentID, tableDesc.Name)
			b.CPut(tKey.Key(), nil, &existingIDVal)
		} else {
			// IMPORT did not create this table, so we should not drop it.
			tableDesc.State = sqlbase.TableDescriptor_PUBLIC
		}
		// Note that this CPut is safe with respect to mixed-version descriptor
		// upgrade and downgrade, because IMPORT does not operate in mixed-version
		// states.
		// TODO(jordan,lucy): remove this comment once 19.2 is released.
		existingDesc, err := sqlbase.ConditionalGetTableDescFromTxn(ctx, txn, tbl.Desc)
		if err != nil {
			return errors.Wrap(err, "rolling back tables")
		}
		b.CPut(
			sqlbase.MakeDescMetadataKey(tableDesc.ID),
			sqlbase.WrapDescriptor(&tableDesc),
			existingDesc)
	}
	return errors.Wrap(txn.Run(ctx, b), "rolling back tables")
}

// OnSuccess is part of the jobs.Resumer interface.
func (r *importResumer) OnSuccess(ctx context.Context, txn *client.Txn) error {
	log.Event(ctx, "making tables live")
	details := r.job.Details().(jobspb.ImportDetails)

	// Needed to trigger the schema change manager.
	if err := txn.SetSystemConfigTrigger(); err != nil {
		return err
	}
	b := txn.NewBatch()
	for _, tbl := range details.Tables {
		tableDesc := *tbl.Desc
		tableDesc.Version++
		tableDesc.State = sqlbase.TableDescriptor_PUBLIC

		if !tbl.IsNew {
			// NB: This is not using AllNonDropIndexes or directly mutating the
			// constraints returned by the other usual helpers because we need to
			// replace the `OutboundFKs` and `Checks` slices of tableDesc with copies
			// that we can mutate. We need to do that because tableDesc is a shallow
			// copy of tbl.Desc that we'll be asserting is the current version when we
			// CPut below.
			//
			// Set FK constraints to unvalidated before publishing the table imported
			// into.
			tableDesc.OutboundFKs = make([]sqlbase.ForeignKeyConstraint, len(tableDesc.OutboundFKs))
			copy(tableDesc.OutboundFKs, tbl.Desc.OutboundFKs)
			for i := range tableDesc.OutboundFKs {
				tableDesc.OutboundFKs[i].Validity = sqlbase.ConstraintValidity_Unvalidated
			}

			// Set CHECK constraints to unvalidated before publishing the table imported into.
			tableDesc.Checks = make([]*sqlbase.TableDescriptor_CheckConstraint, len(tbl.Desc.Checks))
			for i, c := range tbl.Desc.AllActiveAndInactiveChecks() {
				ck := *c
				ck.Validity = sqlbase.ConstraintValidity_Unvalidated
				tableDesc.Checks[i] = &ck
			}
		}

		// TODO(dt): re-validate any FKs?
		// Note that this CPut is safe with respect to mixed-version descriptor
		// upgrade and downgrade, because IMPORT does not operate in mixed-version
		// states.
		// TODO(jordan,lucy): remove this comment once 19.2 is released.
		existingDesc, err := sqlbase.ConditionalGetTableDescFromTxn(ctx, txn, tbl.Desc)
		if err != nil {
			return errors.Wrap(err, "publishing tables")
		}
		b.CPut(
			sqlbase.MakeDescMetadataKey(tableDesc.ID),
			sqlbase.WrapDescriptor(&tableDesc),
			existingDesc)
	}
	if err := txn.Run(ctx, b); err != nil {
		return errors.Wrap(err, "publishing tables")
	}

	// Initiate a run of CREATE STATISTICS. We don't know the actual number of
	// rows affected per table, so we use a large number because we want to make
	// sure that stats always get created/refreshed here.
	for i := range details.Tables {
		r.statsRefresher.NotifyMutation(details.Tables[i].Desc.ID, math.MaxInt32 /* rowsAffected */)
	}

	return nil
}

// OnTerminal is part of the jobs.Resumer interface.
func (r *importResumer) OnTerminal(
	ctx context.Context, status jobs.Status, resultsCh chan<- tree.Datums,
) {
	if status == jobs.StatusSucceeded {
		telemetry.CountBucketed("import.rows", r.res.Rows)
		const mb = 1 << 20
		telemetry.CountBucketed("import.size-mb", r.res.DataSize/mb)

		resultsCh <- tree.Datums{
			tree.NewDInt(tree.DInt(*r.job.ID())),
			tree.NewDString(string(jobs.StatusSucceeded)),
			tree.NewDFloat(tree.DFloat(1.0)),
			tree.NewDInt(tree.DInt(r.res.Rows)),
			tree.NewDInt(tree.DInt(r.res.IndexEntries)),
			tree.NewDInt(tree.DInt(r.res.SystemRecords)),
			tree.NewDInt(tree.DInt(r.res.DataSize)),
		}
	}
}

var _ jobs.Resumer = &importResumer{}

func init() {
	sql.AddPlanHook(importPlanHook)
	jobs.RegisterConstructor(
		jobspb.TypeImport,
		func(job *jobs.Job, settings *cluster.Settings) jobs.Resumer {
			return &importResumer{
				job:      job,
				settings: settings,
			}
		},
	)
}