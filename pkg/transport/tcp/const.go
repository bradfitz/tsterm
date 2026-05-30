package tcp

import "time"

// shutdownTimeout caps how long Serve waits for in-flight handlers when
// its context is canceled.
const shutdownTimeout = 5 * time.Second
