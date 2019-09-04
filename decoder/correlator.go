package decoder

import (
	"bytes"
	"encoding/json"
	"net"
	"strconv"

	"github.com/VictoriaMetrics/fastcache"
	"github.com/negbie/logp"
	"github.com/sipcapture/heplify/protos"
)

var (
	ipPort    bytes.Buffer
	cLine     = []byte("c=IN IP")
	mLine     = []byte("m=audio ")
	aLine     = []byte("a=rtcp:")
	sdpCache  = fastcache.New(30 * 1024 * 1024)
	rtcpCache = fastcache.New(30 * 1024 * 1024)
)

// cacheSDPIPPort will extract the source IP, source Port from SDP body and CallID from SIP header.
// It will do this only for SIP messages which have the strings "c=IN IP4 " and "m=audio " in the SDP body.
// If there is one rtcp attribute in the SDP body it will use it as RTCP port. Otherwise it will add 1 to
// the RTP source port. These data will be used for the SDPCache as key:value pairs.
func cacheSDPIPPort(payload []byte) {
	if posSDPIP := bytes.Index(payload, cLine); posSDPIP > 0 {
		if posSDPPort := bytes.Index(payload, mLine); posSDPPort > 0 {
			ipPort.Reset()
			restIP := payload[posSDPIP:]
			// Minimum IPv4 length of "c=IN IP4 1.1.1.1" = 16
			if posRestIP := bytes.Index(restIP, []byte("\r\n")); posRestIP >= 16 {
				ipPort.Write(restIP[len(cLine)+2 : posRestIP])
			} else {
				logp.Debug("sdp", "No end or fishy SDP IP in '%s'", restIP)
				return
			}

			if posRTCPPort := bytes.Index(payload, aLine); posRTCPPort > 0 {
				restRTCPPort := payload[posRTCPPort:]
				// Minimum RTCP port length of "a=rtcp:1000" = 11
				if posRestRTCPPort := bytes.Index(restRTCPPort, []byte("\r\n")); posRestRTCPPort >= 11 && posRestRTCPPort < 14 {
					ipPort.Write(restRTCPPort[len(aLine):posRestRTCPPort])
				} else if posRestRTCPPort := bytes.IndexRune(restRTCPPort, ' '); posRestRTCPPort >= 11 {
					ipPort.Write(restRTCPPort[len(aLine):posRestRTCPPort])
				} else {
					logp.Debug("sdp", "No end or fishy SDP RTCP Port in '%s'", restRTCPPort)
					return
				}
			} else {
				restPort := payload[posSDPPort:]
				// Minimum RTCP port length of "m=audio 1000" = 12
				if posRestPort := bytes.Index(restPort, []byte(" RTP")); posRestPort >= 12 {
					ipPort.Write(restPort[len(mLine):posRestPort])
					lastNum := len(ipPort.Bytes()) - 1
					ipPort.Bytes()[lastNum] = byte(uint32(ipPort.Bytes()[lastNum]) + 1)
				} else {
					logp.Debug("sdp", "No end or fishy SDP RTP Port in '%s'", restPort)
					return
				}
			}

			var callID []byte
			if posCallID := bytes.Index(payload, []byte("Call-I")); posCallID > 0 {
				restCallID := payload[posCallID:]
				// Minimum Call-ID length of "Call-ID: a" = 10
				if posRestCallID := bytes.Index(restCallID, []byte("\r\n")); posRestCallID >= 10 {
					callID = restCallID[len("Call-ID:"):posRestCallID]
				} else {
					logp.Debug("sdp", "No end or fishy Call-ID in '%s'", restCallID)
					return
				}
			} else if posID := bytes.Index(payload, []byte("i: ")); posID > 0 {
				restID := payload[posID:]
				// Minimum Call-ID length of "i: a" = 4
				if posRestID := bytes.Index(restID, []byte("\r\n")); posRestID >= 4 {
					callID = restID[len("i: "):posRestID]
				} else {
					logp.Debug("sdp", "No end or fishy Call-ID in '%s'", restID)
					return
				}
			} else {
				logp.Warn("No Call-ID in '%s'", payload)
				return
			}

			//logp.Debug("sdp", "Add to SDPCache key=%s, value=%s", ipPort.String(), string(callID))
			sdpCache.Set(ipPort.Bytes(), bytes.TrimSpace(callID))
		}
	}
}

// correlateRTCP will try to correlate RTCP data with SIP messages.
// First it will look inside the longlive RTCPCache with the ssrc as key.
// If it can't find a value it will look inside the shortlive SDPCache with (SDPIP+RTCPPort) as key.
// If it finds a value inside the SDPCache it will add it to the RTCPCache with the ssrc as key.
func correlateRTCP(srcIP net.IP, srcPort uint16, dstIP net.IP, dstPort uint16, payload []byte) ([]byte, []byte) {

	keyRTCP, jsonRTCP, info := protos.ParseRTCP(payload)
	if info != "" {
		logp.Debug("rtcp", "ssrc=%x, srcIP=%s, srcPort=%d, dstIP=%s, dstPort=%d, %s",
			keyRTCP, srcIP, srcPort, dstIP, dstPort, info)
		if jsonRTCP == nil {
			return nil, nil
		}
	}

	if corrID := rtcpCache.Get(nil, keyRTCP); corrID != nil && keyRTCP != nil {
		logp.Debug("rtcp", "Found '%x:%s' in RTCPCache srcIP=%s, srcPort=%d, dstIP=%s, dstPort=%d, payload=%s",
			keyRTCP, corrID, srcIP, srcPort, dstIP, dstPort, jsonRTCP)
		return jsonRTCP, corrID
	}

	srcIPString := srcIP.String()
	srcPortString := strconv.Itoa(int(srcPort))
	srcKey := []byte(srcIPString + srcPortString)
	if corrID := sdpCache.Get(nil, srcKey); corrID != nil {
		logp.Debug("rtcp", "Found '%s:%s' in SDPCache srcIP=%s, srcPort=%s, payload=%s",
			srcKey, corrID, srcIPString, srcPortString, jsonRTCP)
		rtcpCache.Set(keyRTCP, corrID)
		return jsonRTCP, corrID
	}

	dstIPString := dstIP.String()
	dstPortString := strconv.Itoa(int(dstPort))
	dstKey := []byte(dstIPString + dstPortString)
	if corrID := sdpCache.Get(nil, dstKey); corrID != nil {
		logp.Debug("rtcp", "Found '%s:%s' in SDPCache dstIP=%s, dstPort=%s, payload=%s",
			dstKey, corrID, dstIPString, dstPortString, jsonRTCP)
		rtcpCache.Set(keyRTCP, corrID)
		return jsonRTCP, corrID
	}

	logp.Debug("rtcp", "No correlationID for srcIP=%s, srcPort=%s, dstIP=%s, dstPort=%s, payload=%s",
		srcIPString, srcPortString, dstIPString, dstPortString, jsonRTCP)
	return nil, nil
}

func correlateLOG(payload []byte) (byte, []byte) {
	var callID []byte
	if posID := bytes.Index(payload, []byte("ID=")); posID > 0 {
		restID := payload[posID:]
		// Minimum Call-ID length of "ID=a" = 4
		if posRestID := bytes.IndexRune(restID, ' '); posRestID >= 4 {
			callID = restID[len("ID="):posRestID]
		} else if len(restID) > 4 && len(restID) < 80 {
			callID = restID[3:]
		} else {
			logp.Debug("log", "No end or fishy Call-ID in '%s'", restID)
			return 0, nil
		}
		if callID != nil {
			logp.Debug("log", "Found CallID: %s in Logline: '%s'", callID, payload)
			return 100, callID

		}
	} else if posID := bytes.Index(payload, []byte(": [")); posID > 0 {
		restID := payload[posID:]
		if posRestID := bytes.Index(restID, []byte(" port ")); posRestID >= 8 {
			callID = restID[len(": ["):posRestID]
		} else if posRestID := bytes.Index(restID, []byte("]: ")); posRestID >= 4 {
			callID = restID[len(": ["):posRestID]
		} else {
			logp.Debug("log", "No end or fishy Call-ID in '%s'", restID)
			return 0, nil
		}
		if len(callID) > 4 && len(callID) < 80 {
			logp.Debug("log", "Found CallID: %s in Logline: '%s'", callID, payload)
			return 100, callID
		}
	} else if ap := bytes.Index(payload, []byte("alert")); ap > -1 {
		return 112, []byte("alert")
	} else if wp := bytes.Index(payload, []byte("WARN")); wp > -1 {
		return 112, []byte("warning")
	} else if ep := bytes.Index(payload, []byte("ERR")); ep > -1 {
		return 112, []byte("error")
	}
	return 0, nil
}

func correlateNG(payload []byte) ([]byte, []byte) {
	cookie, rawNG, err := unmarshalNG(payload)
	if err != nil {
		logp.Warn("%v", err)
		return nil, nil
	}
	switch rawTypes := rawNG.(type) {
	case map[string]interface{}:
		for rawMapKey, rawMapValue := range rawTypes {
			if rawMapKey == "call-id" {
				callid := rawMapValue.([]byte)
				sipCache.Set(cookie, callid)
			}

			if rawMapKey == "SSRC" {
				data, err := json.Marshal(&rawMapValue)
				if err != nil {
					logp.Warn("%v", err)
					return nil, nil
				}
				if corrID := sipCache.Get(nil, cookie); corrID != nil {
					logp.Debug("ng", "Found CallID: %s and QOS stats: %s", string(corrID), string(data))
					return data, corrID
				}
			}
		}
	}
	return nil, nil
}
