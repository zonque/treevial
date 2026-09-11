package client

import (
	"math"
	"time"
)

// forever is the longest duration gRPC will accept. It is used wherever a
// timeout would otherwise be grounds for closing a connection that gats needs
// to keep open indefinitely.
const forever = time.Duration(math.MaxInt64)
