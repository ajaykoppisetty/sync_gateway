package base

import (
	"log"
	"strings"
	"time"
)

// FlushLogBuffers will cause all log collation buffers to be flushed to the output.
func FlushLogBuffers() {
	time.Sleep(loggerCollateFlushDelay)
}

// logCollationWorker will take log lines over the given channel, and buffer them until either the buffer is full, or the flushTimeout is exceeded.
// This is to reduce the number of writes to the log files, in order to batch them up as larger collated chunks, whilst maintaining a low-level of latency with the flush timeout.
func logCollationWorker(collateBuffer chan string, logger *log.Logger, maxBufferSize int, collateFlushTimeout time.Duration) {
	ticker := time.NewTicker(collateFlushTimeout)
	logBuffer := make([]string, 0, maxBufferSize)

	for {
		select {
		case l := <-collateBuffer:
			logBuffer = append(logBuffer, l)
			// flush if the buffer is full
			if len(logBuffer) >= maxBufferSize {
				logger.Print(strings.Join(logBuffer, "\n"))
				// Empty buffer
				logBuffer = logBuffer[:0]
			}
		case <-ticker.C:
			// flush if we have anything sat in the buffer after this time
			if len(logBuffer) > 0 {
				logger.Print(strings.Join(logBuffer, "\n"))
				// Empty buffer
				logBuffer = logBuffer[:0]
			}
		}
	}
}
