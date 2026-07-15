package sessioncoord

import "context"

// bufferCap is the maximum number of events held in the ordered in-process
// queue. When the buffer is full the goroutine stops reading from the source
// until the consumer catches up (lossless backpressure). Events are runner
// observation patches; an intermediate unique field update may be semantically
// necessary, so drop-old and drop-new policies are both incorrect.
const bufferCap = 256

// bufferEvents starts consuming immediately and provides an ordered lossless
// in-process queue with bounded backpressure. Registration meta I/O therefore
// cannot create a GET-before-subscribe gap, and the buffer cannot grow without
// bound even if the consumer is temporarily blocked.
func bufferEvents(ctx context.Context, in <-chan RunnerEvent) <-chan RunnerEvent {
	out := make(chan RunnerEvent)
	go func() {
		defer close(out)
		var queue []RunnerEvent
		for in != nil || len(queue) != 0 {
			var send chan RunnerEvent
			var first RunnerEvent
			if len(queue) != 0 {
				send, first = out, queue[0]
			}
			// Apply backpressure: stop reading from the source when the
			// buffer is at capacity so the runner stream blocks rather than
			// accumulating events in memory without bound.
			var readIn <-chan RunnerEvent
			if in != nil && len(queue) < bufferCap {
				readIn = in
			}
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-readIn:
				if !ok {
					in = nil
				} else {
					queue = append(queue, ev)
				}
			case send <- first:
				queue = queue[1:]
			}
		}
	}()
	return out
}
