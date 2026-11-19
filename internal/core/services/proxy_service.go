package services

import (
	"context"
	"db-proxy/internal/core/domain"
	"db-proxy/internal/core/ports"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"
)

// ProxyService implements core proxying business logic.
type ProxyService struct {
	logger      ports.QueryLogger
	sysLog      ports.SystemLogger
	broadcaster ports.QueryBroadcaster
	chaosSvc    *ChaosService
}

// NewProxyService creates a new instance of ProxyService.
func NewProxyService(logger ports.QueryLogger, sysLog ports.SystemLogger, broadcaster ports.QueryBroadcaster, chaosSvc *ChaosService) *ProxyService {
	return &ProxyService{
		logger:      logger,
		sysLog:      sysLog,
		broadcaster: broadcaster,
		chaosSvc:    chaosSvc,
	}
}

// HandleConnection forwards TCP data between clientConn and backendAddr.
// It uses protocolParser to extract queries and logs them asynchronously.
func (ps *ProxyService) HandleConnection(ctx context.Context, clientConn net.Conn, backendAddr, connectionName, protocolType string, parser ports.ProtocolParser) {
	defer clientConn.Close()

	// Establish connection to the backend database
	backendConn, err := net.DialTimeout("tcp", backendAddr, 5*time.Second)
	if err != nil {
		ps.sysLog.Error("[%s] Failed to connect to backend %s: %v", connectionName, backendAddr, err)
		return
	}
	defer backendConn.Close()

	clientIP := clientConn.RemoteAddr().String()
	ps.sysLog.Info("[%s] Connection accepted from %s -> forwarding to %s", connectionName, clientIP, backendAddr)

	// Connection-scoped disruption state shared between goroutines
	var connState struct {
		mu               sync.Mutex
		activeDisruption string // "none", "empty", "throttle"
		throttleBytes    int
		throttleInterval time.Duration
		responseDelay    time.Duration
	}
	connState.activeDisruption = "none"

	// Tracker to synchronize query logging with their actual response results.
	var tracker struct {
		mu      sync.Mutex
		pending []domain.LogEntry
		buffer  []byte
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// Goroutine 1: Backend -> Proxy -> Client (intercept server response, parse, disrupt and forward)
	go func() {
		defer wg.Done()
		buffer := make([]byte, 32768)
		for {
			n, err := backendConn.Read(buffer)
			if n > 0 {
				payload := buffer[:n]
				parser.ParseServerResponse(payload)

				connState.mu.Lock()
				disruption := connState.activeDisruption
				tBytes := connState.throttleBytes
				tInterval := connState.throttleInterval
				responseDelay := connState.responseDelay
				connState.mu.Unlock()

				if responseDelay > 0 {
					ps.sysLog.Warn("[%s] Injecting response delay of %dms for %s", connectionName, responseDelay.Milliseconds(), clientIP)
					time.Sleep(responseDelay)
				}

				var bytesToLog []byte
				if disruption == "empty" {
					strippedPayload := stripRowPackets(protocolType, payload)
					if len(strippedPayload) > 0 {
						_, writeErr := clientConn.Write(strippedPayload)
						if writeErr != nil {
							break
						}
						bytesToLog = strippedPayload
					}
				} else if disruption == "throttle" && tBytes > 0 && tInterval > 0 {
					idx := 0
					for idx < len(payload) {
						end := idx + tBytes
						if end > len(payload) {
							end = len(payload)
						}
						chunk := payload[idx:end]
						_, writeErr := clientConn.Write(chunk)
						if writeErr != nil {
							break
						}
						bytesToLog = append(bytesToLog, chunk...)
						idx = end
						time.Sleep(tInterval)
					}
				} else {
					_, writeErr := clientConn.Write(payload)
					if writeErr != nil {
						break
					}
					bytesToLog = payload
				}

				if len(bytesToLog) > 0 {
					tracker.mu.Lock()
					tracker.buffer = append(tracker.buffer, bytesToLog...)
					tracker.mu.Unlock()

					// Try parsing completed responses from the buffer
					for {
						tracker.mu.Lock()
						resText, consumed, ok := parseResponse(protocolType, tracker.buffer)
						if !ok {
							tracker.mu.Unlock()
							break
						}

						// Dequeue the oldest pending query
						var entry domain.LogEntry
						if len(tracker.pending) > 0 {
							entry = tracker.pending[0]
							tracker.pending = tracker.pending[1:]
						}
						tracker.buffer = tracker.buffer[consumed:]
						tracker.mu.Unlock()

						if entry.Query != "" {
							entry.Response = resText
							go func(e domain.LogEntry) {
								if err := ps.logger.LogQuery(e); err != nil {
									ps.sysLog.Error("[%s] Failed to log query: %v", connectionName, err)
								}
								if ps.broadcaster != nil {
									ps.broadcaster.Broadcast(e)
								}
							}(entry)
						}
					}
				}
			}
			if err != nil {
				break
			}
		}
		_ = clientConn.Close()
	}()

	// Goroutine 2: Client -> Proxy -> Backend (intercept, parse query, log, evaluate chaos rules, and forward)
	go func() {
		defer wg.Done()
		buffer := make([]byte, 32768) // 32 KB chunk
		for {
			n, err := clientConn.Read(buffer)
			if n > 0 {
				payload := buffer[:n]

				// Parse and log queries, applying chaos rules dynamically
				queries := parser.Parse(payload)

				var matchedRequestDelay time.Duration
				var matchedResponseDelay time.Duration
				var matchedError string
				var shouldClose bool

				for _, q := range queries {
					loggedQuery := q
					rule, ok := ps.chaosSvc.Evaluate(connectionName, q)

					if ok {
						action := rule.Action
						if action == "error" {
							loggedQuery = fmt.Sprintf("[CHAOS_ERROR: %s] %s", rule.ErrorMsg, q)
						} else if action == "delay" {
							matchedRequestDelay = time.Duration(rule.DelayRequestMs) * time.Millisecond
							matchedResponseDelay = time.Duration(rule.DelayResponseMs) * time.Millisecond
							loggedQuery = fmt.Sprintf("[CHAOS_DELAY: request=%dms response=%dms] %s", rule.DelayRequestMs, rule.DelayResponseMs, q)
						} else if action == "close" {
							loggedQuery = fmt.Sprintf("[CHAOS_CLOSE] %s", q)
						} else if action == "empty" {
							loggedQuery = fmt.Sprintf("[CHAOS_EMPTY] %s", q)
						} else if action == "throttle" {
							loggedQuery = fmt.Sprintf("[CHAOS_THROTTLE: %d B / %dms] %s", rule.ThrottleBytes, rule.ThrottleIntervalMs, q)
						}
					}

					entry := domain.LogEntry{
						Timestamp:  time.Now(),
						ThirdParty: connectionName,
						ClientIP:   clientIP,
						Query:      loggedQuery,
					}

					if ok && (rule.Action == "error" || rule.Action == "close") {
						if rule.Action == "error" {
							entry.Response = "Error: [Chaos Injected] " + rule.ErrorMsg
						} else {
							entry.Response = "[Connection Dropped by Chaos Rule]"
						}
						go func(e domain.LogEntry) {
							if err := ps.logger.LogQuery(e); err != nil {
								ps.sysLog.Error("[%s] Failed to log query: %v", connectionName, err)
							}
							if ps.broadcaster != nil {
								ps.broadcaster.Broadcast(e)
							}
						}(entry)
					} else if protocolType == "iso8583" {
						// ISO 8583 connections are commonly long-lived and a host can defer,
						// omit, or batch a response. Publish the redacted request immediately
						// so dashboard observability never depends on socket closure.
						entry.Response = "[ISO8583 request received; response pending]"
						go func(e domain.LogEntry) {
							if err := ps.logger.LogQuery(e); err != nil {
								ps.sysLog.Error("[%s] Failed to log query: %v", connectionName, err)
							}
							if ps.broadcaster != nil {
								ps.broadcaster.Broadcast(e)
							}
						}(entry)
					} else {
						// Normal query or delay query - enqueue it to wait for response.
						tracker.mu.Lock()
						tracker.pending = append(tracker.pending, entry)
						tracker.mu.Unlock()
					}

					if ok {
						action := rule.Action
						if action == "error" {
							ps.sysLog.Warn("[%s] Injecting error response for query on %s: %s", connectionName, clientIP, rule.ErrorMsg)
							errBytes := parser.BuildErrorResponse(rule.ErrorMsg)
							_, writeErr := clientConn.Write(errBytes)
							if writeErr == nil {
								matchedError = rule.ErrorMsg
							}
							break
						} else if action == "delay" {
							if matchedRequestDelay > 0 {
								ps.sysLog.Warn("[%s] Injecting request delay of %dms for query on %s", connectionName, matchedRequestDelay.Milliseconds(), clientIP)
							}
							connState.mu.Lock()
							connState.responseDelay = matchedResponseDelay
							connState.mu.Unlock()
						} else if action == "close" {
							ps.sysLog.Warn("[%s] Abruptly dropping connection for query on %s", connectionName, clientIP)
							shouldClose = true
							break
						} else if action == "empty" {
							ps.sysLog.Warn("[%s] Injecting empty result set for query on %s", connectionName, clientIP)
							connState.mu.Lock()
							connState.activeDisruption = "empty"
							connState.mu.Unlock()
						} else if action == "throttle" {
							ps.sysLog.Warn("[%s] Throttling response (chunk=%d B, interval=%dms) for query on %s", connectionName, rule.ThrottleBytes, rule.ThrottleIntervalMs, clientIP)
							connState.mu.Lock()
							connState.activeDisruption = "throttle"
							connState.throttleBytes = rule.ThrottleBytes
							if connState.throttleBytes <= 0 {
								connState.throttleBytes = 10
							}
							intervalMs := rule.ThrottleIntervalMs
							if intervalMs <= 0 {
								intervalMs = 100
							}
							connState.throttleInterval = time.Duration(intervalMs) * time.Millisecond
							connState.mu.Unlock()
						}
					} else {
						connState.mu.Lock()
						connState.activeDisruption = "none"
						connState.responseDelay = 0
						connState.mu.Unlock()
					}
				}

				if shouldClose {
					_ = clientConn.Close()
					_ = backendConn.Close()
					return
				}

				if matchedError != "" {
					continue // Skip sending payload to backend database
				}

				if matchedRequestDelay > 0 {
					time.Sleep(matchedRequestDelay)
				}

				// Forward raw bytes to the actual database
				_, writeErr := backendConn.Write(payload)
				if writeErr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		_ = backendConn.Close() // Notify backend of EOF
	}()

	wg.Wait()
	ps.sysLog.Info("[%s] Connection closed for %s", connectionName, clientIP)

	// Clean up any remaining queries in the pending queue
	tracker.mu.Lock()
	for _, entry := range tracker.pending {
		entry.Response = "[Connection Closed]"
		go func(e domain.LogEntry) {
			_ = ps.logger.LogQuery(e)
			if ps.broadcaster != nil {
				ps.broadcaster.Broadcast(e)
			}
		}(entry)
	}
	tracker.pending = nil
	tracker.mu.Unlock()
}

// stripRowPackets removes row data packets from the backend response.
func stripRowPackets(protocolType string, data []byte) []byte {
	switch protocolType {
	case "postgres":
		return stripPostgresRows(data)
	case "mariadb":
		return stripMySQLRows(data)
	case "redis":
		return []byte("*0\r\n") // empty array response
	default:
		return data
	}
}

func stripPostgresRows(data []byte) []byte {
	var res []byte
	idx := 0
	for idx < len(data) {
		if idx+5 > len(data) {
			res = append(res, data[idx:]...)
			break
		}
		msgType := data[idx]
		length := int(binary.BigEndian.Uint32(data[idx+1 : idx+5]))
		if idx+1+length > len(data) {
			res = append(res, data[idx:]...)
			break
		}
		packet := data[idx : idx+1+length]
		if msgType == 'D' {
			// Skip DataRow packet
		} else {
			res = append(res, packet...)
		}
		idx += 1 + length
	}
	return res
}

func stripMySQLRows(data []byte) []byte {
	var res []byte
	idx := 0
	packetCount := 0

	for idx < len(data) {
		if idx+4 > len(data) {
			res = append(res, data[idx:]...)
			break
		}
		length := int(data[idx]) | int(data[idx+1])<<8 | int(data[idx+2])<<16
		if idx+4+length > len(data) {
			res = append(res, data[idx:]...)
			break
		}
		packet := data[idx : idx+4+length]
		payload := data[idx+4 : idx+4+length]

		isRow := false
		if packetCount > 0 && len(payload) > 0 {
			firstByte := payload[0]
			isOK := firstByte == 0x00 && length >= 7
			isEOF := firstByte == 0xfe && length < 9
			isERR := firstByte == 0xff
			isColDef := false
			if len(payload) >= 4 && payload[0] == 0x03 && payload[1] == 'd' && payload[2] == 'e' && payload[3] == 'f' {
				isColDef = true
			}

			if !isOK && !isEOF && !isERR && !isColDef {
				isRow = true
			}
		}

		if !isRow {
			res = append(res, packet...)
		}
		idx += 4 + length
		packetCount++
	}
	return res
}
