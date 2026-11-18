# Common-delivery pacing maximum: negative SDK result

Both frozen arms include the prefix timing correction, accepted SDK timing contracts and pending-drain ring guards. The after arm changes only the pacing basis to the greater of positive service and common raw delivery, before the existing margin and target bound. It leaves sizing and backlog classification unchanged.

Each arm passes 183 repeated race checks. Each passes the unchanged SDK Short group and fails SDK Long in both the fixed 100 ms and 400 ms device-default cells. The one-line maximum is therefore not a startup fix and is not adopted.

All 40 model readings, 370 top-level outcomes, deterministic diagnostic lines and original source/build/binary/run hashes are retained. The original warmups, durations and capacity/queue/refusal gates remain unchanged. Runs began immediately under recorded concurrent load. No quality-change event was injected.

Final service exceeds the common rate in the failing SDK cells. The next experiment traces the early pacing/window limiter; a static-path failure remains part of core acceptance.
