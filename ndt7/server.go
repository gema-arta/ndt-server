package ndt7

import (
	"crypto/rand"
	"net/http"
	"strconv"
	"time"

	"github.com/apex/log"
	"github.com/gorilla/websocket"
	"github.com/m-lab/ndt-cloud/bbr"
)

// defaultDuration is the default duration of a subtest in nanoseconds.
const defaultDuration = 10 * time.Second

// maxDuration is the maximum duration of a subtest in seconds
const maxDuration = 30

// DownloadHandler handles a download subtest from the server side.
type DownloadHandler struct {
	Upgrader websocket.Upgrader
}

// stoppableAccordingToBW returns true when we can stop the current download
// test based on |prev|, the previous BBR bandwidth sample, and |cur| the
// current BBR bandwidth sample. This algorithm runs every 0.25 seconds and
// indicates that the download can stop if the bandwidth estimated using
// BBR stops growing. We use the same percentage used by the BBR paper
// to characterize the bandwidth growth, i.e. 25%. The BBR paper can be
// read online at <https://queue.acm.org/detail.cfm?id=3022184>.
func stoppableAccordingToBW(prev float64, cur float64) bool {
	return cur >= prev && (cur - prev) < (0.25 * prev)
}

// Handle handles the download subtest.
func (dl DownloadHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	log.Debug("Processing query string")
	duration := defaultDuration
	{
		s := request.URL.Query().Get("duration")
		if s != "" {
			value, err := strconv.Atoi(s)
			if err != nil || value < 0 || value > maxDuration {
				log.Warn("The duration option has an invalid value")
				writer.Header().Set("Connection", "Close")
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			duration = time.Second * time.Duration(value)
		}
	}
	log.Debug("Upgrading to WebSockets")
	if request.Header.Get("Sec-WebSocket-Protocol") != SecWebSocketProtocol {
		log.Warn("Missing Sec-WebSocket-Protocol in request")
		writer.Header().Set("Connection", "Close")
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	headers := http.Header{}
	headers.Add("Sec-WebSocket-Protocol", SecWebSocketProtocol)
	conn, err := dl.Upgrader.Upgrade(writer, request, headers)
	if err != nil {
		log.WithError(err).Warn("upgrader.Upgrade() failed")
		return
	}
	// TODO(bassosimone): currently we're leaking filedesc cache entries if we
	// error out before this point. Because we have concluded that the cache
	// cannot grow indefinitely, this is probably not a priority.
	//
	// We don't care much about an error here because fd is -1 on error and we
	// will check later whether |fd| is different from that value.
	fd, _ := bbr.ExtractBBRFd(conn.LocalAddr().String())
	conn.SetReadLimit(MinMaxMessageSize)
	defer conn.Close()
	log.Debug("Generating random buffer")
	const bufferSize = 1 << 13
	data := make([]byte, bufferSize)
	rand.Read(data)
	buffer, err := websocket.NewPreparedMessage(websocket.BinaryMessage, data)
	if err != nil {
		log.WithError(err).Warn("websocket.NewPreparedMessage() failed")
		return
	}
	log.Debug("Start sending data to client")
	ticker := time.NewTicker(MinMeasurementInterval)
	defer ticker.Stop()
	t0 := time.Now()
	count := int64(0)
	bandwidth := float64(0)
	for running := true; running; {
		select {
		case t := <-ticker.C:
			// TODO(bassosimone): here we should also include tcp_info data
			// TODO(bassosimone): here we should also include BBR data
			measurement := Measurement{
				Elapsed:  t.Sub(t0).Nanoseconds(),
				NumBytes: count,
			}
			if fd != -1 {
				bw, rtt, err := bbr.GetBBRInfo(fd)
				if err == nil {
					// Implementation note: the linux kernel header seems to suggest
					// the measurement unit of the RTT is nsec, however, both empirical
					// evidence and github.com/mikioh/tcpinfo [1] suggest that the RTT
					// is actually in microseconds.
					//
					// [1] See https://github.com/mikioh/tcpinfo/blob/131b59fef27f73876a7760a644c1e08cf585075c/sys_linux.go#L313
					log.Infof("BW: %f bytes/s; RTT: %f usec", bw, rtt)
					// TODO(bassosimone): This algorithm is currently enabled by
					// default by we should actually make it conditional.
					running = !stoppableAccordingToBW(bandwidth, bw)
					if !running {
						log.Info("It seems bandwidth has stopped growing")
					}
					bandwidth = bw
				}
			}
			conn.SetWriteDeadline(time.Now().Add(defaultTimeout))
			if err := conn.WriteJSON(&measurement); err != nil {
				log.WithError(err).Warn("Cannot send measurement message")
				return
			}
		default: // Not ticking, just send more data
			if time.Now().Sub(t0) >= duration {
				running = false
				break
			}
			conn.SetWriteDeadline(time.Now().Add(defaultTimeout))
			if err := conn.WritePreparedMessage(buffer); err != nil {
				log.WithError(err).Warn("cannot send data message")
				return
			}
			count += bufferSize
		}
	}
	log.Debug("Closing the WebSocket connection")
	conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(
		websocket.CloseNormalClosure, ""), time.Now().Add(defaultTimeout))
}
