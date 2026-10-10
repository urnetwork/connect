# service receiver owned interval rejected v1

This prototype is rejected. It attempted to pair each sequence with its preceding corrected cumulative checkpoint and pool the resulting intervals. Both matched versions still fail the lawful batched consumer root. The diagnostic identifies a same-endpoint veto, but removing that veto would not make the interval sound: a cumulative head can carry an ingress time before some prefix bytes, and local H1 FIFO does not imply destination FIFO. No production adoption or further guard bypass occurred. The later raw aggregate design avoids this corrected-prefix premise.

Original full source/build/runtime CWD, binary, status and raw-log hashes remain pinned by each normalized manifest. Original local files are unchanged.
