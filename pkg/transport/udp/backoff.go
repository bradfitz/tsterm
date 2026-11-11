package udp

import (
	"math/rand/v2"
	"time"
)

// GiveUpAfter is how long either side keeps retransmitting to a silent
// peer before going quiet. The server then holds its undelivered screen
// state until the client is heard from again; the client drops its
// undelivered keystrokes. Either way, an unreachable peer (such as a phone
// in a pocket) stops costing radio time after this long.
const GiveUpAfter = 60 * time.Second

// Backoff returns a randomized wait before retransmission number attempt,
// counting from 0. The wait doubles with each attempt, starting from base
// and capped at limit, and is then jittered uniformly to between half and
// one and a half times that, so that peers retrying after a shared outage
// do not stay in lockstep.
func Backoff(base, limit time.Duration, attempt int) time.Duration {
	d := max(base, time.Millisecond)
	for i := 0; i < attempt && d < limit; i++ {
		d *= 2
	}
	d = min(d, limit)
	return d/2 + rand.N(d)
}
