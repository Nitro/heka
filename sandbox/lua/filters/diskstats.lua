-- This Source Code Form is subject to the terms of the Mozilla Public
-- License, v. 2.0. If a copy of the MPL was not distributed with this
-- file, You can obtain one at http://mozilla.org/MPL/2.0/.

--[[
Graphs the disk IO stats on the system running heka. It automatically converts
the running totals of Writes and Reads into rates of the values. The time based
fields are left as running totals of the amount of time doing IO.

Config:

- rows (uint, optional, default 1440)
    Sets the size of the sliding window i.e., 1440 rows representing 60 seconds
    per row is a 24 sliding hour window with 1 minute resolution.

- anomaly_config(string) - (see :ref:`sandbox_anomaly_module`)

*Example Heka Configuration*

.. code-block:: ini

    [DiskStatsFilter]
    type = "SandboxFilter"
    filename = "lua_filters/diskstats.lua"
    preserve_data = true
    message_matcher = "Type == 'stats.diskstats'"

--]]

require "circular_buffer"
require "string"

local alert         = require "alert"
local annotation    = require "annotation"
local anomaly       = require "anomaly"

last_run                = { rows = nil, sec_per_row = nil}
local rows              = read_config("rows") or 1440
local sec_per_row       = nil
local per_unit          = nil

local anomaly_config    = anomaly.parse_config(read_config("anomaly_config"))

stats = {
    disk_stats = {
        cbuf = nil,
        fields = {
            { name = "WritesCompleted", key = "Fields[WritesCompleted]", last_value = nil },
            { name = "ReadsCompleted", key = "Fields[ReadsCompleted]", last_value = nil },
            { name = "SectorsWritten", key = "Fields[SectorsWritten]", last_value = nil },
            { name = "SectorsRead", key = "Fields[SectorsRead]", last_value = nil },
            { name = "WritesMerged", key = "Fields[WritesMerged]", last_value = nil },
            { name = "ReadsMerged", key = "Fields[ReadsMerged]", last_value = nil },
        },
        title = "Disk Stats",
        unit = "",
        aggregation = "none"
    },
    time_stats = {
        cbuf = nil,
        fields = {
            { name = "TimeWriting", key = "Fields[TimeWriting]" },
            { name = "TimeReading", key = "Fields[TimeReading]" },
            { name = "TimeDoingIO", key = "Fields[TimeDoingIO]" },
            { name = "WeightedTimeDoingIO", key = "Fields[WeightedTimeDoingIO]" },
        },
        title = "Time doing IO",
        unit = "ms",
        aggregation = "max"
    }
}

local function init_cbuf(stat)
    local cbuf = circular_buffer.new(rows, #stat.fields, sec_per_row)
    for i, field in ipairs(stat.fields) do
        cbuf:set_header(i, field.name, stat.unit, stat.aggregation)
    end
    annotation.set_prune(stat.title, rows * sec_per_row * 1e9)
    stat.cbuf = cbuf
    -- Set our persisted globals
    last_run.rows = rows
    last_run.sec_per_row = sec_per_row
    return cbuf
end

local function update_sec_per_row()
    sec_per_row = read_message("Fields[TickerInterval]")
    if stats.disk_stats.unit == "" then
        stats.disk_stats.unit = "per_" .. sec_per_row .. "_s"
    end
    return sec_per_row
end

-- This tells us if our persisted settings changed between runs.
local function settings_changed()
    update_sec_per_row()
    if (rows == last_run.rows) and (sec_per_row == last_run.sec_per_row) then
        return false
    end
    return true
end


function process_message ()
    local ts = read_message("Timestamp")
    if sec_per_row == nil then
        update_sec_per_row()
    end

    local time_stats = stats.time_stats
    if not time_stats.cbuf then
        init_cbuf(time_stats)
    end

    for i, v in ipairs(time_stats.fields) do
        local val = read_message(v.key)
        if type(val) ~= "number" then return -1 end
        time_stats.cbuf:set(ts, i, val)
    end

    -- Set up our Cbuf if not setup yet
    local disk_stats = stats.disk_stats
    local cb = stats.disk_stats.cbuf
    if not cb or settings_changed() then
        init_cbuf(disk_stats)
    end

    for i, v in ipairs(disk_stats.fields) do
        local val = read_message(v.key)
        if type(val) ~= "number" then return -1 end
        if v.last_value ~= nil then
            -- FIXME: Causes a spike when there is a delayed restart w/ preservation = true
            -- Compute delta
            local delta = val - v.last_value
            disk_stats.cbuf:set(ts, i, delta)
        end
        v.last_value = val
    end

    return 0
end

function timer_event(ns)
    if anomaly_config then
        for i, stat in ipairs(stats) do
            local title = stat.title
            local buf = stat.cbuf
            if not alert.throttled(ns) then
                local msg, annos = anomaly.detect(ns, title, buf, anomaly_config)
                if msg then
                    annotation.concat(buf, annos)
                    alert.queue(ns, msg)
                end
            end
            inject_payload("cbuf", title, annotation.prune(title, ns), buf)
        end
    else
        for k, stat in pairs(stats) do
            inject_payload("cbuf", stat.title, stat.cbuf)
        end
    end
    alert.send_queue(ns)
end
