-- This Source Code Form is subject to the terms of the Mozilla Public
-- License, v. 2.0. If a copy of the MPL was not distributed with this
-- file, You can obtain one at http://mozilla.org/MPL/2.0/.

--[=[
Extracts data from message fields in `heka.statmetric` messages generated by a
:ref:`config_stat_accum_input` and generates JSON suitable for use with
InfluxDB's `HTTP API
<http://influxdb.com/docs/v0.7/api/reading_and_writing_data.html>`_.
StatAccumInput must be configured with `emit_in_fields = true` for this
encoder to work correctly.

*Example Heka Configuration*

.. code-block:: ini

    [statmetric-influx-encoder]
    type = "SandboxEncoder"
    filename = "lua_encoders/statmetric_influx.lua"

    [influx]
    type = "HttpOutput"
    message_matcher = "Type == 'heka.statmetric'"
    address = "http://myinfluxserver.example.com:8086/db/stats/series"
    encoder = "statmetric-influx-encoder"
    username = "influx_username"
    password = "influx_password"

*Example Output*

.. code-block:: json

    [{"points":[[1408404848,78271]],"name":"stats.counters.000000.rate","columns":["time","value"]},{"points":[[1408404848,78271]],"name":"stats.counters.000000.count","columns":["time","value"]},{"points":[[1408404848,17420]],"name":"stats.timers.000001.count","columns":["time","value"]},{"points":[[1408404848,17420]],"name":"stats.timers.000001.count_ps","columns":["time","value"]},{"points":[[1408404848,1]],"name":"stats.timers.000001.lower","columns":["time","value"]},{"points":[[1408404848,1024]],"name":"stats.timers.000001.upper","columns":["time","value"]},{"points":[[1408404848,8937851]],"name":"stats.timers.000001.sum","columns":["time","value"]},{"points":[[1408404848,513.07985074627]],"name":"stats.timers.000001.mean","columns":["time","value"]},{"points":[[1408404848,461.72356167879]],"name":"stats.timers.000001.mean_90","columns":["time","value"]},{"points":[[1408404848,925]],"name":"stats.timers.000001.upper_90","columns":["time","value"]},{"points":[[1408404848,2]],"name":"stats.statsd.numStats","columns":["time","value"]}]

--]=]

require "cjson"

function process_message()
    local output = {}
    local ts = tonumber(read_message("Fields[timestamp]"))
    if not ts then return -1 end
    ts = ts * 1000
    while true do
        typ, name, value, representation, count = read_next_field()
        if not typ then break end

        if name ~= "timestamp" and typ ~= 1 then -- exclude bytes
            local stat = {
                name = name,
                columns = {"time", "value"},
                points = {{ts, value}}
            }
            output[#output+1] = stat
        end
    end
    inject_payload("json", "influx_stats", cjson.encode(output))
    return 0
end
