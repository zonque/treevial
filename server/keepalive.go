package server

import (
	"math"
	"time"
)

// forever is the longest duration gRPC will accept. It stands in for "never"
// wherever a keepalive setting would otherwise close a connection that treevial
// needs to stay open indefinitely.
const forever = time.Duration(math.MaxInt64)

// minPingInterval is the shortest gap the server accepts between a client's
// keepalive pings. It is the smallest non-zero value, because zero would mean
// "use gRPC's five-minute default" and any faster pinger would then be cut off
// with GOAWAY ENHANCE_YOUR_CALM.
const minPingInterval = time.Nanosecond
