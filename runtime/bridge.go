package remote

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/OpenSlash/agent-bridge/internal/applog"
	"github.com/OpenSlash/agent-bridge/protocol"

	"github.com/gorilla/websocket"
)

// startBridge 启动 stdout→WS + WS→stdin 双向桥接
func (s *Service) startBridge() {
	sessionID := s.sessionID

	if !s.cfg.Management && s.cmd != nil && s.stdin != nil && s.stdout != nil {
		s.startProcessBridge(s.cmd, s.stdin, s.stdout, sessionID)
	}

	// WS → stdin：接收用户输入，转换为 stream-json 格式写入 stdin
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		historyPusher := newSessionHistoryPusher(s.cfg.ServerURL, s.cfg.Token, s.contentProtector)
		inputSeq := int64(0)
		for {
			_, data, err := s.conn.ReadMessage()
			if err != nil {
				s.markDisconnected()
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
					applog.Errorf("[Remote] WS read error: %v", err)
				}
				return
			}

			var msg protocol.Message
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}

			switch msg.Type {
			case protocol.TypeInput:
				if s.cfg.Management {
					continue
				}
				var input protocol.InputPayload
				if s.decodePayload(sessionID, msg.Type, msg.Payload, &input) == nil {
					if attachedInput := s.getAttachedInputHandler(); attachedInput != nil {
						if err := attachedInput(input.Data); err != nil {
							applog.Errorf("[Remote] attached input error: %v", err)
							_ = s.writeJSON(protocol.Message{
								Type:      protocol.TypeError,
								SessionID: sessionID,
								Payload: protocol.ErrorPayload{
									Message: stringsTrimRightNewlines(err.Error()),
								},
							})
							continue
						}
						content := stringsTrimRightNewlines(input.Data)
						if content != "" {
							applog.Info.Printf("[Remote] attached input received: session=%s chars=%d", sessionID, len(content))
							inputSeq++
						}
						if err := s.beginAttachedTurn(sessionID); err != nil {
							applog.Errorf("[Remote] attached turn-start error: %v", err)
						}
						continue
					}
					handled, handleErr := s.handleLocalSlashCommand(sessionID, input.Data, historyPusher)
					if handleErr != nil {
						applog.Errorf("[Remote] local slash command error: %v", handleErr)
						continue
					}
					if handled {
						continue
					}
					if writeErr := s.writeUserMessage(input.Data); writeErr != nil {
						applog.Errorf("[Remote] stdin write error: %v", writeErr)
						_ = s.writeJSON(protocol.Message{
							Type:      protocol.TypeText,
							SessionID: sessionID,
							Payload: protocol.TextPayload{
								Data:     fmt.Sprintf("{\"type\":\"system\",\"subtype\":\"error\",\"message\":\"%s\"}", stringsTrimRightNewlines(writeErr.Error())),
								Thinking: false,
							},
						})
						continue
					}
					content := stringsTrimRightNewlines(input.Data)
					if content != "" {
						applog.Info.Printf("[Remote] input received: session=%s chars=%d", sessionID, len(content))
						inputSeq++
					}

					s.beginTurn()

					turnMsg := protocol.Message{
						Type:      protocol.TypeTurnStart,
						SessionID: sessionID,
						Payload: protocol.TurnStartPayload{
							TurnID: fmt.Sprintf("turn-%d", time.Now().UnixMilli()),
						},
					}
					if err := s.writeJSON(turnMsg); err != nil {
						applog.Errorf("[Remote] WS write turn-start error: %v", err)
						return
					}
				}
			case protocol.TypeControl:
				if s.cfg.Management {
					continue
				}
				var ctrl protocol.ControlPayload
				if s.decodePayload(sessionID, msg.Type, msg.Payload, &ctrl) == nil {
					switch ctrl.Action {
					case protocol.ActionInterrupt:
						if attachedInterrupt := s.getAttachedInterruptHandler(); attachedInterrupt != nil {
							if err := attachedInterrupt(); err != nil {
								applog.Errorf("[Remote] attached interrupt failed: %v", err)
							} else {
								s.MarkAttachedInterruptRequested()
							}
							continue
						}
						if err := s.requestInterrupt(); err != nil {
							applog.Errorf("[Remote] interrupt request failed: %v", err)
						}
					default:
						applog.Info.Printf("[Remote] Control: %s", ctrl.Action)
					}
				}
			case protocol.TypeListDir:
				var req protocol.ListDirPayload
				if s.decodePayload(sessionID, msg.Type, msg.Payload, &req) == nil {
					go s.handleListDir(sessionID, req)
				}
			case protocol.TypeReadFile:
				var req protocol.ReadFilePayload
				if s.decodePayload(sessionID, msg.Type, msg.Payload, &req) == nil {
					go s.handleReadFile(sessionID, req)
				}
			case protocol.TypeGitStatus:
				var req protocol.GitStatusPayload
				if s.decodePayload(sessionID, msg.Type, msg.Payload, &req) == nil {
					go s.handleGitStatus(sessionID, req)
				}
			case protocol.TypeGitDiff:
				var req protocol.GitDiffPayload
				if s.decodePayload(sessionID, msg.Type, msg.Payload, &req) == nil {
					go s.handleGitDiff(sessionID, req)
				}
			case protocol.TypeGitLog:
				var req protocol.GitLogPayload
				if s.decodePayload(sessionID, msg.Type, msg.Payload, &req) == nil {
					go s.handleGitLog(sessionID, req)
				}
			case protocol.TypeGitCommitDetail:
				var req protocol.GitCommitDetailPayload
				if s.decodePayload(sessionID, msg.Type, msg.Payload, &req) == nil {
					go s.handleGitCommitDetail(sessionID, req)
				}
			case protocol.TypeListCommands:
				var req protocol.ListCommandsPayload
				if s.decodePayload(sessionID, msg.Type, msg.Payload, &req) == nil {
					go s.handleListCommands(sessionID, req)
				}
			case protocol.TypeListSkills:
				var req protocol.ListSkillsPayload
				if s.decodePayload(sessionID, msg.Type, msg.Payload, &req) == nil {
					go s.handleListSkills(sessionID, req)
				}
			case protocol.TypeSessionKeyRequest:
				var req protocol.SessionKeyRequestPayload
				if decodePlainPayload(msg.Payload, &req) == nil {
					applog.Info.Printf(
						"[Remote] session-key-request received: session=%s request=%s scope=%s management=%t",
						sessionID,
						req.RequestID,
						req.Scope,
						s.cfg.Management,
					)
					go s.handleSessionKeyRequest(sessionID, req)
				}
			case protocol.TypeSessionConfig:
				if s.cfg.Management {
					continue
				}
				var cfg protocol.SessionConfigPayload
				if s.decodePayload(sessionID, msg.Type, msg.Payload, &cfg) == nil {
					if attachedConfig := s.getAttachedConfigHandler(); attachedConfig != nil {
						if err := attachedConfig(cfg); err != nil {
							applog.Errorf("[Remote] attached session-config failed: %v", err)
						}
						if err := s.sendCurrentKeepalive(sessionID); err != nil {
							applog.Errorf("[Remote] keepalive after attached session-config failed: %v", err)
						}
						continue
					}
					decision := s.evaluateSessionConfig(cfg)
					applog.Info.Printf(
						"[Remote] session-config received: session=%s runtime=%s permission=%s->%s sandbox=%s->%s restart=%t applyModel=%t targetModel=%s",
						sessionID,
						s.getRuntime(),
						s.getCurrentPermissionMode(),
						decision.TargetPermissionMode,
						s.getCurrentSandboxMode(),
						decision.TargetSandboxMode,
						decision.NeedsRestart,
						decision.ApplyModel,
						decision.TargetModel,
					)
					if decision.PermissionModeChanged && !decision.PermissionModeNeedsRestart {
						s.setCurrentPermissionMode(decision.TargetPermissionMode)
					}
					if decision.SandboxModeChanged && !decision.SandboxModeNeedsRestart {
						s.setCurrentSandboxMode(decision.TargetSandboxMode)
					}
					if (decision.PermissionModeChanged && !decision.PermissionModeNeedsRestart) ||
						(decision.SandboxModeChanged && !decision.SandboxModeNeedsRestart) {
						shouldSendKeepalive := true
						if s.getRuntime() == runtimeCodex {
							applied, err := s.rebindCodexThreadConfiguration()
							if err != nil {
								applog.Errorf("[Remote] codex execution mode rebind failed: %v", err)
							}
							shouldSendKeepalive = applied
						}
						if shouldSendKeepalive {
							if err := s.sendCurrentKeepalive(sessionID); err != nil {
								applog.Errorf("[Remote] keepalive after execution mode switch failed: %v", err)
							}
						} else {
							applog.Info.Printf(
								"[Remote] keepalive deferred until codex execution mode rebind applies: session=%s runtime=%s permission=%s sandbox=%s",
								sessionID,
								s.getRuntime(),
								s.getCurrentPermissionMode(),
								s.getCurrentSandboxMode(),
							)
						}
					}
					if decision.NeedsRestart {
						if err := s.restartCommand(
							sessionID,
							cfg.WorkingDir,
							decision.TargetModel,
							decision.ApplyModel,
							decision.TargetPermissionMode,
							decision.TargetSandboxMode,
							decision.ResumeConversation,
						); err != nil {
							applog.Errorf("[Remote] restart command failed: %v", err)
						}
						continue
					}
				}
			case protocol.TypeCreateSession:
				var req protocol.CreateSessionPayload
				if s.decodePayload(sessionID, msg.Type, msg.Payload, &req) == nil {
					go s.handleCreateSession(sessionID, req)
				}
			case protocol.TypeSessionAction:
				var req protocol.SessionActionPayload
				if s.decodePayload(sessionID, msg.Type, msg.Payload, &req) == nil {
					go s.handleSessionAction(sessionID, req)
				}
			case protocol.TypePermissionResponse:
				if s.cfg.Management {
					continue
				}
				var resp protocol.PermissionResponsePayload
				if s.decodePayload(sessionID, msg.Type, msg.Payload, &resp) == nil {
					applog.Info.Printf(
						"[Remote] permission response received: session=%s request=%s decision=%s approved=%t has_input=%t",
						sessionID,
						resp.RequestID,
						resp.Decision,
						resp.Approved,
						resp.UpdatedInput != nil,
					)
					s.resolvePermissionResponse(resp)
				}
			}
		}
	}()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		lastPing := time.Time{}
		for {
			select {
			case <-s.done:
				return
			case <-ticker.C:
				if err := s.sendCurrentKeepalive(sessionID); err != nil {
					s.markDisconnected()
					return
				}
				if lastPing.IsZero() || time.Since(lastPing) >= relayPingInterval {
					if err := s.writePing(); err != nil {
						s.markDisconnected()
						return
					}
					lastPing = time.Now()
				}
			}
		}
	}()
}

func stringsTrimRightNewlines(value string) string {
	for len(value) > 0 {
		last := value[len(value)-1]
		if last != '\n' && last != '\r' {
			break
		}
		value = value[:len(value)-1]
	}
	return value
}
