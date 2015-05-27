// Generated by tsc.
// source: cockroach/resource/us/ts/...
// DO NOT EDIT!
//
// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Authors: Bram Gruneir (bram.gruneir@gmail.com)
//		    Andrew Bonventre (andybons@gmail.com)
//		    Matt Tracy (matt@cockroachlabs.com)
//
var headerDescription = 'This file is designed to add the header to the top of the combined js file.';
// source: models/timeseries.ts
/// <reference path="../typings/mithriljs/mithril.d.ts" />
// Author: Matt Tracy (matt@cockroachlabs.com)
var Models;
(function (Models) {
    var Metrics;
    (function (Metrics) {
        (function (QueryAggregator) {
            QueryAggregator[QueryAggregator["AVG"] = 1] = "AVG";
            QueryAggregator[QueryAggregator["AVG_RATE"] = 2] = "AVG_RATE";
        })(Metrics.QueryAggregator || (Metrics.QueryAggregator = {}));
        var QueryAggregator = Metrics.QueryAggregator;
        function query(start, end, series) {
            var url = "/ts/query";
            var data = {
                start_nanos: start.getTime() * 1.0e6,
                end_nanos: end.getTime() * 1.0e6,
                queries: series.map(function (r) { return { name: r }; }),
            };
            return m.request({ url: url, method: "POST", extract: nonJsonErrors, data: data })
                .then(function (d) {
                if (!d.results) {
                    d.results = [];
                }
                d.results.forEach(function (r) {
                    if (!r.datapoints) {
                        r.datapoints = [];
                    }
                });
                return d;
            });
        }
        var RecentQuery = (function () {
            function RecentQuery(windowDuration) {
                var series = [];
                for (var _i = 1; _i < arguments.length; _i++) {
                    series[_i - 1] = arguments[_i];
                }
                this.windowDuration = windowDuration;
                this._series = series;
            }
            RecentQuery.prototype.query = function () {
                var endTime = new Date();
                var startTime = new Date(endTime.getTime() - this.windowDuration);
                return query(startTime, endTime, this._series);
            };
            return RecentQuery;
        })();
        Metrics.RecentQuery = RecentQuery;
        var QueryManager = (function () {
            function QueryManager(_query) {
                this._query = _query;
                this._result = null;
                this._error = null;
                this._resultEpoch = 0;
                this._outstanding = null;
            }
            QueryManager.prototype.processOutstanding = function () {
                if (this._outstanding) {
                    var completed = (this._outstanding.error() != null || this._outstanding.result() != null);
                    if (completed) {
                        this._result = this._outstanding.result();
                        this._error = this._outstanding.error();
                        this._outstanding = null;
                        this._resultEpoch++;
                    }
                }
            };
            QueryManager.prototype.result = function () {
                this.processOutstanding();
                return this._result;
            };
            QueryManager.prototype.epoch = function () {
                this.processOutstanding();
                return this._resultEpoch;
            };
            QueryManager.prototype.error = function () {
                this.processOutstanding();
                return this._error;
            };
            QueryManager.prototype.refresh = function () {
                this.result();
                if (!this._outstanding) {
                    this._outstanding = {
                        result: this._query.query(),
                        error: m.prop(null),
                    };
                    this._outstanding.result.then(null, this._outstanding.error);
                }
                return this._outstanding.result;
            };
            return QueryManager;
        })();
        Metrics.QueryManager = QueryManager;
        function nonJsonErrors(xhr, opts) {
            return xhr.status > 200 ? JSON.stringify(xhr.responseText) : xhr.responseText;
        }
    })(Metrics = Models.Metrics || (Models.Metrics = {}));
})(Models || (Models = {}));
// source: components/metrics.ts
/// <reference path="../typings/mithriljs/mithril.d.ts" />
/// <reference path="../typings/d3/d3.d.ts" />
/// <reference path="../models/timeseries.ts" />
var Components;
(function (Components) {
    var Metrics;
    (function (Metrics) {
        var LineGraph;
        (function (LineGraph) {
            var Controller = (function () {
                function Controller(vm) {
                    var _this = this;
                    this.vm = vm;
                    this.chart = nv.models.lineChart()
                        .x(function (d) { return new Date(d.timestamp_nanos / 1.0e6); })
                        .y(function (d) { return d.value; })
                        .useInteractiveGuideline(true)
                        .showLegend(true)
                        .showYAxis(true)
                        .showXAxis(true)
                        .xScale(d3.time.scale());
                    this.drawGraph = function (element, isInitialized, context) {
                        if (!isInitialized) {
                            nv.addGraph(_this.chart);
                        }
                        if (_this.shouldRenderData()) {
                            var formattedData = [];
                            if (_this.vm.query.result()) {
                                formattedData = _this.vm.query.result().results.map(function (d) {
                                    return {
                                        values: d.datapoints,
                                        key: d.name,
                                        color: Controller.colors(d.name),
                                        area: true,
                                        fillOpacity: .1,
                                    };
                                });
                            }
                            d3.select(element)
                                .datum(formattedData)
                                .transition().duration(500)
                                .call(_this.chart);
                        }
                    };
                    this.chart.xAxis
                        .tickFormat(d3.time.format('%I:%M:%S'))
                        .showMaxMin(false);
                }
                Controller.prototype.shouldRenderData = function () {
                    var epoch = this.vm.query.epoch();
                    if (epoch > this.vm.lastEpoch) {
                        this.vm.lastEpoch = epoch;
                        return true;
                    }
                    return false;
                };
                Controller.prototype.hasData = function () {
                    return this.vm.query.epoch() > 0;
                };
                Controller.colors = d3.scale.category10();
                return Controller;
            })();
            function controller(model) {
                return new Controller(model);
            }
            LineGraph.controller = controller;
            function view(ctrl) {
                if (ctrl.hasData()) {
                    return m(".linegraph", { style: "width:500px;height:300px;" }, m("svg.graph", { config: ctrl.drawGraph }));
                }
                else {
                    return m("", "loading...");
                }
            }
            LineGraph.view = view;
            function create(query, key) {
                var vm = { lastEpoch: 0, query: query };
                if (key) {
                    vm.key = key;
                }
                return m.component(LineGraph, vm);
            }
            LineGraph.create = create;
        })(LineGraph = Metrics.LineGraph || (Metrics.LineGraph = {}));
    })(Metrics = Components.Metrics || (Components.Metrics = {}));
})(Components || (Components = {}));
// source: pages/graph.ts
/// <reference path="../typings/mithriljs/mithril.d.ts" />
/// <reference path="../typings/d3/d3.d.ts" />
/// <reference path="../models/timeseries.ts" />
/// <reference path="../components/metrics.ts" />
var AdminViews;
(function (AdminViews) {
    var Graph;
    (function (Graph) {
        var Page;
        (function (Page) {
            function controller() {
                var query = new Models.Metrics.RecentQuery(10 * 60 * 1000, "cr.node.calls.success.1");
                var manager = new Models.Metrics.QueryManager(query);
                manager.refresh();
                var interval = setInterval(function () { return manager.refresh(); }, 10000);
                return {
                    manager: manager,
                    onunload: function () { return clearInterval(interval); },
                };
            }
            Page.controller = controller;
            function view(ctrl) {
                var windowSize = 10 * 60 * 1000;
                return m(".graphPage", [
                    m("H3", "Graph Demo"),
                    Components.Metrics.LineGraph.create(ctrl.manager),
                    Components.Metrics.LineGraph.create(ctrl.manager),
                ]);
            }
            Page.view = view;
        })(Page = Graph.Page || (Graph.Page = {}));
    })(Graph = AdminViews.Graph || (AdminViews.Graph = {}));
})(AdminViews || (AdminViews = {}));
// source: pages/monitor.ts
/// <reference path="../typings/mithriljs/mithril.d.ts" />
var AdminViews;
(function (AdminViews) {
    var Monitor;
    (function (Monitor) {
        var Page;
        (function (Page) {
            function controller() { }
            Page.controller = controller;
            function view() {
                return m("h3", "Monitor Placeholder");
            }
            Page.view = view;
        })(Page = Monitor.Page || (Monitor.Page = {}));
    })(Monitor = AdminViews.Monitor || (AdminViews.Monitor = {}));
})(AdminViews || (AdminViews = {}));
// source: models/stats.ts
/// <reference path="../typings/mithriljs/mithril.d.ts" />
// Author: Bram Gruneir (bram.gruneir@gmail.com)
// source: models/node_status.ts
/// <reference path="../typings/mithriljs/mithril.d.ts" />
/// <reference path="stats.ts" />
// Author: Bram Gruneir (bram.gruneir@gmail.com)
var Models;
(function (Models) {
    var NodeStatus;
    (function (NodeStatus) {
        var Nodes = (function () {
            function Nodes() {
                this._data = m.prop({});
                this.desc = m.prop({});
                this.statuses = m.prop({});
            }
            Nodes.prototype.Query = function () {
                var _this = this;
                var url = "/_status/nodes/";
                return m.request({ url: url, method: "GET", extract: nonJsonErrors })
                    .then(function (results) {
                    results.d.forEach(function (status) {
                        if (_this._data()[status.desc.node_id] == null) {
                            _this._data()[status.desc.node_id] = [];
                        }
                        _this._data()[status.desc.node_id].push(status);
                        _this.statuses()[status.desc.node_id] = status;
                    });
                    _this._pruneOldEntries();
                    _this._updateDescriptions();
                    return results;
                });
            };
            Nodes.prototype._updateDescriptions = function () {
                this.desc({});
                var nodeId;
                for (nodeId in this._data()) {
                    this.desc()[nodeId] = this._data()[nodeId][this._data()[nodeId].length - 1].desc;
                }
            };
            Nodes.prototype._pruneOldEntries = function () {
                var nodeId;
                for (nodeId in this._data()) {
                    var status = this._data()[nodeId];
                    if (status.length > Nodes._dataLimit) {
                        status = status.sclice(status.length - Nodes._dataPrunedSize, status.length - 1);
                    }
                }
            };
            Nodes._dataLimit = 100000;
            Nodes._dataPrunedSize = 90000;
            return Nodes;
        })();
        NodeStatus.Nodes = Nodes;
        function nonJsonErrors(xhr, opts) {
            return xhr.status > 200 ? JSON.stringify(xhr.responseText) : xhr.responseText;
        }
    })(NodeStatus = Models.NodeStatus || (Models.NodeStatus = {}));
})(Models || (Models = {}));
// source: pages/nodes.ts
/// <reference path="../typings/mithriljs/mithril.d.ts" />
/// <reference path="../models/node_status.ts" />
var AdminViews;
(function (AdminViews) {
    var Nodes;
    (function (Nodes) {
        Nodes.nodeStatuses = new Models.NodeStatus.Nodes();
        var Controller = (function () {
            function Controller() {
                Nodes.nodeStatuses.Query();
                this._interval = setInterval(function () { return Nodes.nodeStatuses.Query(); }, Controller._queryEveryMS);
            }
            Controller.prototype.onunload = function () {
                clearInterval(this._interval);
            };
            Controller._queryEveryMS = 10000;
            return Controller;
        })();
        Nodes.Controller = Controller;
        var NodesPage;
        (function (NodesPage) {
            function controller() {
                return new Controller();
            }
            NodesPage.controller = controller;
            function view() {
                return m("div", [
                    m("h2", "Nodes List"),
                    m("ul", [
                        Object.keys(Nodes.nodeStatuses.desc()).sort().map(function (nodeId) {
                            var desc = Nodes.nodeStatuses.desc()[nodeId];
                            return m("li", { key: desc.node_id }, m("div", [
                                m.trust("&nbsp;&bull;&nbsp;"),
                                m("a[href=/nodes/" + desc.node_id + "]", { config: m.route }, "Node:" + desc.node_id),
                                " with Address:" + desc.address.network + "-" + desc.address.address
                            ]));
                        }),
                    ]),
                ]);
            }
            NodesPage.view = view;
        })(NodesPage = Nodes.NodesPage || (Nodes.NodesPage = {}));
        var NodePage;
        (function (NodePage) {
            function controller() {
                return new Controller();
            }
            NodePage.controller = controller;
            function view() {
                var nodeId = m.route.param("node_id");
                return m("div", [
                    m("h2", "Node Status"),
                    m("div", [
                        m("h3", "Node: " + nodeId),
                        m("p", JSON.stringify(Nodes.nodeStatuses.statuses()[nodeId]))
                    ])
                ]);
            }
            NodePage.view = view;
        })(NodePage = Nodes.NodePage || (Nodes.NodePage = {}));
    })(Nodes = AdminViews.Nodes || (AdminViews.Nodes = {}));
})(AdminViews || (AdminViews = {}));
// source: pages/rest_explorer.ts
/// <reference path="../typings/mithriljs/mithril.d.ts" />
var AdminViews;
(function (AdminViews) {
    var RestExplorer;
    (function (RestExplorer) {
        var Model;
        (function (Model) {
            Model.singleKey = m.prop("");
            Model.singleValue = m.prop("");
            Model.singleCounter = m.prop(0);
            Model.rangeStart = m.prop("");
            Model.rangeEnd = m.prop("");
            Model.responseLog = m.prop([]);
            function logResponse(xhr, opts) {
                var data;
                if (xhr.responseType === "json") {
                    data = JSON.stringify(xhr.response);
                }
                else {
                    data = xhr.responseText;
                }
                data = data.length > 0 ? data : "(no response body)";
                data = ['[', opts.method, '] ', xhr.status, ' ', opts.url, ': ', data].join('');
                Model.responseLog().push(data);
                return JSON.stringify(data);
            }
            function scan(method) {
                var endpoint = "/kv/rest/range?start=" + encodeURIComponent(Model.rangeStart());
                if (!!Model.rangeEnd()) {
                    endpoint += '&end=' + encodeURIComponent(Model.rangeEnd());
                }
                return m.request({
                    method: method,
                    url: endpoint,
                    extract: logResponse,
                });
            }
            Model.scan = scan;
            function entry(method) {
                var endpoint = "/kv/rest/entry/" + Model.singleKey();
                var request = {
                    method: method,
                    url: endpoint,
                    extract: logResponse,
                    serialize: function (data) { return data; },
                };
                if (method === "POST") {
                    request.config = function (xhr, opts) {
                        xhr.setRequestHeader("Content-Type", "text/plain; charset=UTF-8");
                        return xhr;
                    };
                    request.data = Model.singleValue();
                }
                return m.request(request);
            }
            Model.entry = entry;
            function counter(method) {
                var endpoint = "/kv/rest/counter/" + Model.singleKey();
                var request = {
                    method: method,
                    url: endpoint,
                    extract: logResponse,
                    serialize: function (data) { return data; },
                };
                if (method === "POST") {
                    request.config = function (xhr, opts) {
                        xhr.setRequestHeader("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8");
                        return xhr;
                    };
                    request.data = Model.singleCounter();
                }
                return m.request(request);
            }
            Model.counter = counter;
            function clearLog() {
                Model.responseLog([]);
            }
            Model.clearLog = clearLog;
            ;
        })(Model || (Model = {}));
        function button(text, onclick, disabled) {
            return m("input[type=button]", {
                value: text,
                disabled: disabled(),
                onclick: onclick,
            });
        }
        function field(text, value, disabled) {
            return m("input[type=text]", {
                placeholder: text,
                disabled: disabled(),
                value: value(),
                onchange: m.withAttr("value", value),
            });
        }
        var EntryComponent;
        (function (EntryComponent) {
            var Controller = (function () {
                function Controller() {
                    var _this = this;
                    this.responsePending = m.prop(false);
                    this.key = Model.singleKey;
                    this.val = Model.singleValue;
                    this.complete = function () { return _this.responsePending(false); };
                    this.get = function () { return _this.request("GET"); };
                    this.post = function () { return _this.request("POST"); };
                    this.head = function () { return _this.request("HEAD"); };
                    this.delete = function () { return _this.request("DELETE"); };
                }
                Controller.prototype.request = function (method) {
                    this.responsePending(true);
                    Model.entry(method).then(this.complete, this.complete);
                };
                return Controller;
            })();
            function controller() {
                return new Controller();
            }
            EntryComponent.controller = controller;
            function view(ctrl) {
                return m("section.restExplorerControls-control", [
                    m("h3", "K/V Pair"),
                    m("form", [
                        field("Key", ctrl.key, ctrl.responsePending),
                        m.trust("&rarr;"),
                        field("Value", ctrl.val, ctrl.responsePending),
                        button("Get", ctrl.get, ctrl.responsePending),
                        button("Head", ctrl.head, ctrl.responsePending),
                        button("Put", ctrl.post, ctrl.responsePending),
                        button("Delete", ctrl.delete, ctrl.responsePending),
                    ])
                ]);
            }
            EntryComponent.view = view;
        })(EntryComponent || (EntryComponent = {}));
        var RangeComponent;
        (function (RangeComponent) {
            var Controller = (function () {
                function Controller() {
                    var _this = this;
                    this.responsePending = m.prop(false);
                    this.rangeStart = Model.rangeStart;
                    this.rangeEnd = Model.rangeEnd;
                    this.complete = function () { return _this.responsePending(false); };
                    this.get = function () { return _this.request("GET"); };
                    this.delete = function () { return _this.request("DELETE"); };
                }
                Controller.prototype.request = function (method) {
                    this.responsePending(true);
                    Model.scan(method).then(this.complete, this.complete);
                };
                return Controller;
            })();
            function controller() {
                return new Controller();
            }
            RangeComponent.controller = controller;
            function view(ctrl) {
                return m("section.restExplorerControls-control", [
                    m("h3", "Range"),
                    m("form", [
                        field("Start", ctrl.rangeStart, ctrl.responsePending),
                        m.trust("&rarr;"),
                        field("End", ctrl.rangeEnd, ctrl.responsePending),
                        button("Get", ctrl.get, ctrl.responsePending),
                        button("Delete", ctrl.delete, ctrl.responsePending),
                    ])
                ]);
            }
            RangeComponent.view = view;
        })(RangeComponent || (RangeComponent = {}));
        var CounterComponent;
        (function (CounterComponent) {
            var Controller = (function () {
                function Controller() {
                    var _this = this;
                    this.responsePending = m.prop(false);
                    this.key = Model.singleKey;
                    this.val = Model.singleCounter;
                    this.complete = function () { return _this.responsePending(false); };
                    this.get = function () { return _this.request("GET"); };
                    this.post = function () { return _this.request("POST"); };
                    this.head = function () { return _this.request("HEAD"); };
                    this.delete = function () { return _this.request("DELETE"); };
                }
                Controller.prototype.request = function (method) {
                    this.responsePending(true);
                    Model.counter(method).then(this.complete, this.complete);
                };
                return Controller;
            })();
            function controller() {
                return new Controller();
            }
            CounterComponent.controller = controller;
            function view(ctrl) {
                return m("section.restExplorerControls-control", [
                    m("h3", "Counter"),
                    m("form", [
                        field("Key", ctrl.key, ctrl.responsePending),
                        m.trust("&rarr;"),
                        field("Value", ctrl.val, ctrl.responsePending),
                        button("Get", ctrl.get, ctrl.responsePending),
                        button("Head", ctrl.head, ctrl.responsePending),
                        button("Put", ctrl.post, ctrl.responsePending),
                        button("Delete", ctrl.delete, ctrl.responsePending),
                    ])
                ]);
            }
            CounterComponent.view = view;
        })(CounterComponent || (CounterComponent = {}));
        var LogComponent;
        (function (LogComponent) {
            function controller() {
                return {
                    log: Model.responseLog,
                    clear: Model.clearLog,
                };
            }
            LogComponent.controller = controller;
            function view(ctrl) {
                return m(".restExplorerLog", [
                    m("h3", "Console"),
                    button("Clear", ctrl.clear, function () { return false; }),
                    ctrl.log().map(function (str) {
                        return m("", str);
                    })
                ]);
            }
            LogComponent.view = view;
        })(LogComponent || (LogComponent = {}));
        var Page;
        (function (Page) {
            function controller() { }
            Page.controller = controller;
            function view() {
                return m(".restExplorer", [
                    m(".restExplorerControls", [
                        EntryComponent,
                        RangeComponent,
                        CounterComponent,
                    ]),
                    LogComponent,
                ]);
            }
            Page.view = view;
        })(Page = RestExplorer.Page || (RestExplorer.Page = {}));
    })(RestExplorer = AdminViews.RestExplorer || (AdminViews.RestExplorer = {}));
})(AdminViews || (AdminViews = {}));
// source: models/store_status.ts
/// <reference path="../typings/mithriljs/mithril.d.ts" />
/// <reference path="node_status.ts" />
/// <reference path="stats.ts" />
// Author: Bram Gruneir (bram.gruneir@gmail.com)
var Models;
(function (Models) {
    var StoreStatus;
    (function (StoreStatus) {
        var Stores = (function () {
            function Stores() {
                this._data = m.prop({});
                this.desc = m.prop({});
                this.statuses = m.prop({});
            }
            Stores.prototype.Query = function () {
                var _this = this;
                var url = "/_status/stores/";
                return m.request({ url: url, method: "GET", extract: nonJsonErrors })
                    .then(function (results) {
                    results.d.forEach(function (status) {
                        if (_this._data()[status.desc.store_id] == null) {
                            _this._data()[status.desc.store_id] = [];
                        }
                        _this._data()[status.desc.store_id].push(status);
                        _this.statuses()[status.desc.store_id] = status;
                    });
                    _this._pruneOldEntries();
                    _this._updateDescriptions();
                    return results;
                });
            };
            Stores.prototype._updateDescriptions = function () {
                this.desc({});
                var nodeId;
                for (nodeId in this._data()) {
                    this.desc()[nodeId] = this._data()[nodeId][this._data()[nodeId].length - 1].desc;
                }
            };
            Stores.prototype._pruneOldEntries = function () {
                var nodeId;
                for (nodeId in this._data()) {
                    var status = this._data()[nodeId];
                    if (status.length > Stores._dataLimit) {
                        status = status.sclice(status.length - Stores._dataPrunedSize, status.length - 1);
                    }
                }
            };
            Stores._dataLimit = 100000;
            Stores._dataPrunedSize = 90000;
            return Stores;
        })();
        StoreStatus.Stores = Stores;
        function nonJsonErrors(xhr, opts) {
            return xhr.status > 200 ? JSON.stringify(xhr.responseText) : xhr.responseText;
        }
    })(StoreStatus = Models.StoreStatus || (Models.StoreStatus = {}));
})(Models || (Models = {}));
// source: pages/stores.ts
/// <reference path="../typings/mithriljs/mithril.d.ts" />
/// <reference path="../models/store_status.ts" />
var AdminViews;
(function (AdminViews) {
    var Stores;
    (function (Stores) {
        Stores.storeStatuses = new Models.StoreStatus.Stores();
        var Controller = (function () {
            function Controller() {
                Stores.storeStatuses.Query();
                this._interval = setInterval(function () { return Stores.storeStatuses.Query(); }, Controller._queryEveryMS);
            }
            Controller.prototype.onunload = function () {
                clearInterval(this._interval);
            };
            Controller._queryEveryMS = 10000;
            return Controller;
        })();
        Stores.Controller = Controller;
        var StoresPage;
        (function (StoresPage) {
            function controller() {
                return new Controller();
            }
            StoresPage.controller = controller;
            function view() {
                return m("div", [
                    m("h2", "Stores List"),
                    m("ul", [
                        Object.keys(Stores.storeStatuses.desc()).sort().map(function (storeId) {
                            var desc = Stores.storeStatuses.desc()[storeId];
                            return m("li", { key: desc.store_id }, m("div", [
                                m.trust("&nbsp;&bull;&nbsp;"),
                                m("a[href=/stores/" + storeId + "]", { config: m.route }, "Store:" + storeId),
                                " on ",
                                m("a[href=/nodes/" + desc.node.node_id + "]", { config: m.route }, "Node:" + desc.node.node_id),
                                " with Address:" + desc.node.address.network + "-" + desc.node.address.address
                            ]));
                        }),
                    ]),
                ]);
            }
            StoresPage.view = view;
        })(StoresPage = Stores.StoresPage || (Stores.StoresPage = {}));
        var StorePage;
        (function (StorePage) {
            function controller() {
                return new Controller();
            }
            StorePage.controller = controller;
            function view() {
                var storeId = m.route.param("store_id");
                return m("div", [
                    m("h2", "Store Status"),
                    m("div", [
                        m("h3", "Store: " + storeId),
                        m("p", JSON.stringify(Stores.storeStatuses.statuses()[storeId]))
                    ])
                ]);
            }
            StorePage.view = view;
        })(StorePage = Stores.StorePage || (Stores.StorePage = {}));
    })(Stores = AdminViews.Stores || (AdminViews.Stores = {}));
})(AdminViews || (AdminViews = {}));
// source: app.ts
/// <reference path="typings/mithriljs/mithril.d.ts" />
/// <reference path="pages/graph.ts" />
/// <reference path="pages/monitor.ts" />
/// <reference path="pages/nodes.ts" />
/// <reference path="pages/rest_explorer.ts" />
/// <reference path="pages/stores.ts" />
m.route.mode = "hash";
m.route(document.getElementById("root"), "/rest-explorer", {
    "/graph": AdminViews.Graph.Page,
    "/monitor": AdminViews.Monitor.Page,
    "/node": AdminViews.Nodes.NodesPage,
    "/nodes": AdminViews.Nodes.NodesPage,
    "/node/:node_id": AdminViews.Nodes.NodePage,
    "/nodes/:node_id": AdminViews.Nodes.NodePage,
    "/rest-explorer": AdminViews.RestExplorer.Page,
    "/store": AdminViews.Stores.StorePage,
    "/stores": AdminViews.Stores.StoresPage,
    "/store/:store_id": AdminViews.Stores.StorePage,
    "/stores/:store_id": AdminViews.Stores.StorePage
});
