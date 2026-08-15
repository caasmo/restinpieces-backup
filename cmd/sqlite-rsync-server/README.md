Run these in two terminals, from the repo root:

Terminal 1 — origin server:

    RIP_BCK_ORIGIN_LISTEN_ADDR=127.0.0.1:9909 RIP_BCK_ORIGIN_FILE=/tmp/opencode/origin.db go run ./cmd/sqlite-rsync-server
    
Terminal 2 — client (local mode):

    RIP_BCK_REPLICA_LABEL=db RIP_BCK_REPLICA_DIR=/tmp/opencode/replica go run ./cmd/sqlite-rsync-client -l
