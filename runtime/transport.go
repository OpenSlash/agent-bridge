package remote

import (
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/OpenSlash/agent-bridge/protocol"

	"github.com/gorilla/websocket"
)

func (s *Service) writeJSON(msg protocol.Message) error {
	plainMsg := msg

	s.mu.Lock()
	sink := s.sink
	localMode := s.localMode
	s.mu.Unlock()

	if !localMode && s.contentProtector != nil {
		protectedPayload, err := s.contentProtector.ProtectPayload(msg.SessionID, msg.Type, msg.Payload, s.cfg.Management)
		if err != nil {
			return err
		}
		msg.Payload = protectedPayload
	}

	if sink != nil {
		if err := sink(msg); err != nil {
			return err
		}
		s.notifyMessageObservers(plainMsg)
		return nil
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if err := s.writeMessage(websocket.TextMessage, data); err != nil {
		return err
	}
	s.notifyMessageObservers(plainMsg)
	return nil
}

func (s *Service) writeMessage(messageType int, data []byte) error {
	s.wsMu.Lock()
	defer s.wsMu.Unlock()

	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("websocket is not connected")
	}

	if err := conn.SetWriteDeadline(time.Now().Add(relayWriteTimeout)); err != nil {
		return err
	}
	defer conn.SetWriteDeadline(time.Time{})
	return conn.WriteMessage(messageType, data)
}

func (s *Service) writePing() error {
	return s.writeMessage(websocket.PingMessage, nil)
}

func configureRelayConnection(conn *websocket.Conn) {
	if conn == nil {
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(relayPongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(relayPongWait))
	})
}

func (s *Service) decodePayload(targetID, messageType string, payload any, out any) error {
	if s.contentProtector == nil {
		return decodePlainPayload(payload, out)
	}
	return s.contentProtector.DecodePayload(targetID, messageType, payload, s.cfg.Management, out)
}

func buildWSURL(serverURL, path, token string) (string, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return "", err
	}

	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "ws", "wss":
	default:
		u.Scheme = "ws"
	}

	u.Path = path
	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func buildHTTPURL(serverURL, path, token string) (string, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return "", err
	}

	switch u.Scheme {
	case "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	case "http", "https":
	default:
		u.Scheme = "http"
	}

	u.Path = path
	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
