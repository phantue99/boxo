package gateway

import (
	"io"

	"github.com/juju/ratelimit"
)

var RLBucket = ratelimit.NewBucketWithRate(1000*1024, 1024*1024)

func RateLimitReader(isDedicatedGateway bool, r io.Reader) io.Reader {
	if isDedicatedGateway {
		return r
	}
	return ratelimit.Reader(r, RLBucket)
}
