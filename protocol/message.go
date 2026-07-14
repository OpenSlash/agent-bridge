package protocol

import "time"

// 消息类型常量
const (
	TypeText              = "text"               // 终端输出（电脑→手机）
	TypeInput             = "input"              // 用户输入（手机→电脑）
	TypeControl           = "control"            // 控制权切换
	TypeKeepalive         = "keepalive"          // 心跳
	TypeTurnStart         = "turn-start"         // Claude 回合开始
	TypeTurnEnd           = "turn-end"           // Claude 回合结束
	TypeSessions          = "sessions"           // 会话列表响应
	TypeHosts             = "hosts"              // 主机列表响应
	TypeError             = "error"              // 错误
	TypeRegister          = "register"           // 电脑端注册会话
	TypeRegistered        = "registered"         // 注册成功响应
	TypeManagerRegister   = "manager-register"   // 主机管理端注册
	TypeManagerRegistered = "manager-registered" // 主机管理注册成功响应

	TypePermissionRequest  = "permission-request"  // 权限请求（电脑→手机）
	TypePermissionResponse = "permission-response" // 权限响应（手机→电脑）
	TypePermissionCleared  = "permission-cleared"  // 权限请求已取消/清理（电脑→手机）

	TypeSessionConfig       = "session-config"        // 会话配置（手机→电脑）
	TypeCreateSession       = "create-session"        // 创建新代理会话（手机→电脑）
	TypeSessionCreated      = "session-created"       // 新代理会话创建结果（电脑→手机）
	TypeSessionAction       = "session-action"        // 会话操作请求（手机→电脑）
	TypeSessionActionResult = "session-action-result" // 会话操作结果（电脑→手机）
	TypeSessionLifecycle    = "session-lifecycle"     // 会话生命周期事件（电脑→手机）

	TypeListDir                 = "list-dir"                   // 目录浏览请求（手机→电脑）
	TypeListDirResponse         = "list-dir-response"          // 目录浏览响应（电脑→手机）
	TypeReadFile                = "read-file"                  // 文件预览请求（手机→电脑）
	TypeReadFileResponse        = "read-file-response"         // 文件预览响应（电脑→手机）
	TypeGitStatus               = "git-status"                 // Git 仓库状态请求（手机→电脑）
	TypeGitStatusResponse       = "git-status-response"        // Git 仓库状态响应（电脑→手机）
	TypeGitDiff                 = "git-diff"                   // Git diff 请求（手机→电脑）
	TypeGitDiffResponse         = "git-diff-response"          // Git diff 响应（电脑→手机）
	TypeGitLog                  = "git-log"                    // Git 提交记录请求（手机→电脑）
	TypeGitLogResponse          = "git-log-response"           // Git 提交记录响应（电脑→手机）
	TypeGitCommitDetail         = "git-commit-detail"          // Git 提交详情请求（手机→电脑）
	TypeGitCommitDetailResponse = "git-commit-detail-response" // Git 提交详情响应（电脑→手机）
	TypeListCommands            = "list-commands"              // Slash 命令列表请求（手机→电脑）
	TypeListCommandsResponse    = "list-commands-response"     // Slash 命令列表响应（电脑→手机）
	TypeListSkills              = "list-skills"                // Codex 技能列表请求（手机→电脑）
	TypeListSkillsResponse      = "list-skills-response"       // Codex 技能列表响应（电脑→手机）
	TypeSessionKeyRequest       = "session-key-request"        // 会话内容密钥请求（手机→电脑）
	TypeSessionKeyResponse      = "session-key-response"       // 会话内容密钥响应（电脑→手机）
)

// Claude 会话权限模式
const (
	PermissionModeDefault           = "default"
	PermissionModeAcceptEdits       = "acceptEdits"
	PermissionModePlan              = "plan"
	PermissionModeDontAsk           = "dontAsk"
	PermissionModeBypassPermissions = "bypassPermissions"
)

// 沙箱模式
const (
	SandboxModeReadOnly         = "read-only"
	SandboxModeWorkspaceWrite   = "workspace-write"
	SandboxModeDangerFullAccess = "danger-full-access"
)

// 权限请求决策
const (
	PermissionDecisionApproved           = "approved"
	PermissionDecisionApprovedForSession = "approved_for_session"
	PermissionDecisionDenied             = "denied"
	PermissionDecisionAbort              = "abort"
)

// 会话管理动作
const (
	SessionActionInterrupt = "interrupt"
	SessionActionPause     = "pause"
	SessionActionStop      = "stop"
	SessionActionDelete    = "delete"
)

// 会话生命周期状态
const (
	SessionLifecycleStarting     = "starting"
	SessionLifecycleActive       = "active"
	SessionLifecycleInterrupting = "interrupting"
	SessionLifecyclePausing      = "pausing"
	SessionLifecyclePaused       = "paused"
	SessionLifecycleStopping     = "stopping"
	SessionLifecycleStopped      = "stopped"
	SessionLifecycleDeleting     = "deleting"
	SessionLifecycleDeleted      = "deleted"
	SessionLifecycleFailed       = "failed"
	SessionLifecycleReconnecting = "reconnecting"
	SessionLifecycleResumed      = "resumed"
)

// 控制动作
const (
	ActionTake      = "take"      // 手机端获取控制权
	ActionRelease   = "release"   // 手机端释放控制权
	ActionInterrupt = "interrupt" // 中断当前 Claude 回合
)

// 回合结束状态
const (
	TurnCompleted = "completed"
	TurnFailed    = "failed"
	TurnCancelled = "cancelled"
)

// Message WS 消息信封
type Message struct {
	Type      string `json:"type"`
	SessionID string `json:"sid,omitempty"`
	Payload   any    `json:"payload,omitempty"`
}

// RegisterPayload 电脑端注册会话
type RegisterPayload struct {
	SessionID        string              `json:"session_id,omitempty"`
	RuntimeSessionID string              `json:"runtime_session_id,omitempty"`
	Hostname         string              `json:"hostname"`
	Cwd              string              `json:"cwd"`
	Pid              int                 `json:"pid"`
	HostID           string              `json:"host_id,omitempty"`
	Command          string              `json:"command,omitempty"`         // 启动的命令，如 "claude"
	OS               string              `json:"os,omitempty"`              // 操作系统，如 "darwin", "linux"
	Version          string              `json:"version,omitempty"`         // CLI 版本
	Model            string              `json:"model,omitempty"`           // 当前模型
	PermissionMode   string              `json:"permission_mode,omitempty"` // 当前权限模式
	SandboxMode      string              `json:"sandbox_mode,omitempty"`    // 当前沙箱模式
	RuntimeCatalog   []RuntimeCapability `json:"runtime_catalog,omitempty"`
}

// RegisteredPayload 注册成功响应
type RegisteredPayload struct {
	SessionID string `json:"session_id"`
}

// ManagerRegisteredPayload 主机管理注册成功响应
type ManagerRegisteredPayload struct {
	HostID string `json:"host_id"`
}

// TextPayload 终端输出
type TextPayload struct {
	Data     string `json:"data"`     // base64 编码的终端输出
	Thinking bool   `json:"thinking"` // Claude 是否在思考中
}

// InputPayload 用户输入
type InputPayload struct {
	Data        string                       `json:"data"` // 原始输入文本
	Attachments []CreateSessionAttachmentRef `json:"attachments,omitempty"`
}

// ControlPayload 控制权切换
type ControlPayload struct {
	Action string `json:"action"` // take / release
}

// KeepalivePayload 心跳
type KeepalivePayload struct {
	Thinking         bool                `json:"thinking"`
	Mode             string              `json:"mode"`                         // local / remote
	Model            string              `json:"model,omitempty"`              // 当前模型（可能在会话中切换）
	Cwd              string              `json:"cwd,omitempty"`                // 当前工作目录
	RuntimeSessionID string              `json:"runtime_session_id,omitempty"` // runtime 内部会话 ID，如 Codex thread id
	PermissionMode   string              `json:"permission_mode,omitempty"`    // 当前权限模式
	SandboxMode      string              `json:"sandbox_mode,omitempty"`       // 当前沙箱模式
	RuntimeCatalog   []RuntimeCapability `json:"runtime_catalog,omitempty"`
}

// TurnStartPayload 回合开始
type TurnStartPayload struct {
	TurnID string `json:"turn_id"`
}

// TurnEndPayload 回合结束
type TurnEndPayload struct {
	TurnID string `json:"turn_id"`
	Status string `json:"status"` // completed / failed / cancelled
}

// SessionInfo 会话信息
type SessionInfo struct {
	SessionID                  string    `json:"session_id"`
	RuntimeSessionID           string    `json:"runtime_session_id,omitempty"`
	HostID                     string    `json:"host_id,omitempty"`
	Hostname                   string    `json:"hostname"`
	Cwd                        string    `json:"cwd"`
	Pid                        int       `json:"pid"`
	Command                    string    `json:"command,omitempty"`
	Thinking                   bool      `json:"thinking"`
	Online                     bool      `json:"online"`
	Mode                       string    `json:"mode"`
	CreatedAt                  time.Time `json:"created_at"`
	OS                         string    `json:"os,omitempty"`
	Version                    string    `json:"version,omitempty"`
	Model                      string    `json:"model,omitempty"`
	PermissionMode             string    `json:"permission_mode,omitempty"`
	SandboxMode                string    `json:"sandbox_mode,omitempty"`
	StopReason                 string    `json:"stop_reason,omitempty"`
	PendingPermissionRequestID string    `json:"pending_permission_request_id,omitempty"`
	PendingPermissionTool      string    `json:"pending_permission_tool,omitempty"`
	PendingPermissionSummary   string    `json:"pending_permission_summary,omitempty"`
	PendingPermissionCreatedAt int64     `json:"pending_permission_created_at,omitempty"`
}

// SessionsPayload 会话列表
type SessionsPayload struct {
	Sessions []SessionInfo `json:"sessions"`
}

// RuntimeModelInfo 主机声明的模型目录项
type RuntimeModelInfo struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
}

// RuntimeCapability 主机声明的运行时能力
type RuntimeCapability struct {
	ID             string             `json:"id"`
	Title          string             `json:"title,omitempty"`
	Models         []RuntimeModelInfo `json:"models,omitempty"`
	SupportsImages bool               `json:"supports_images,omitempty"`
}

// HostInfo 远程管理主机信息
type HostInfo struct {
	HostID         string              `json:"host_id"`
	Hostname       string              `json:"hostname"`
	Cwd            string              `json:"cwd"`
	Online         bool                `json:"online"`
	CreatedAt      time.Time           `json:"created_at"`
	OS             string              `json:"os,omitempty"`
	Version        string              `json:"version,omitempty"`
	RuntimeCatalog []RuntimeCapability `json:"runtime_catalog,omitempty"`
}

// HostsPayload 主机列表
type HostsPayload struct {
	Hosts []HostInfo `json:"hosts"`
}

// ErrorPayload 错误
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// PermissionRequestPayload 权限请求（电脑→手机）
type PermissionRequestPayload struct {
	RequestID string `json:"request_id"`
	Tool      string `json:"tool"`
	Input     any    `json:"input"`   // tool 参数
	Summary   string `json:"summary"` // 人类可读摘要
	CreatedAt int64  `json:"created_at"`
}

// PermissionResponsePayload 权限响应（手机→电脑）
type PermissionResponsePayload struct {
	RequestID    string         `json:"request_id"`
	Approved     bool           `json:"approved"`
	Decision     string         `json:"decision"`                // approved / approved_for_session / denied / abort
	UpdatedInput map[string]any `json:"updated_input,omitempty"` // optional modified tool input
}

// PermissionClearedPayload 权限请求清理（电脑→手机）
type PermissionClearedPayload struct {
	RequestID string `json:"request_id"`
}

// SessionConfigPayload 会话配置（手机→电脑，连接后发送）
type SessionConfigPayload struct {
	WorkingDir     string `json:"working_dir,omitempty"`     // 工作目录
	Model          string `json:"model,omitempty"`           // 模型
	ApplyModel     bool   `json:"apply_model,omitempty"`     // 是否显式应用模型（允许切回默认）
	PermissionMode string `json:"permission_mode,omitempty"` // 权限模式
	SandboxMode    string `json:"sandbox_mode,omitempty"`    // 沙箱模式
	Restart        bool   `json:"restart,omitempty"`         // 是否重启 Claude 进程创建新会话
}

// CreateSessionPayload 创建新代理会话（手机→电脑）
type CreateSessionPayload struct {
	Version                int                          `json:"version,omitempty"`
	RequestID              string                       `json:"request_id,omitempty"`
	IdempotencyKey         string                       `json:"idempotency_key,omitempty"`
	WorkingDir             string                       `json:"working_dir,omitempty"`
	Runtime                string                       `json:"runtime,omitempty"`
	Model                  string                       `json:"model,omitempty"`
	PermissionMode         string                       `json:"permission_mode,omitempty"`
	SandboxMode            string                       `json:"sandbox_mode,omitempty"`
	ResumeSessionID        string                       `json:"resume_session_id,omitempty"`
	ResumeRuntimeSessionID string                       `json:"resume_runtime_session_id,omitempty"`
	InitialPrompt          string                       `json:"initial_prompt,omitempty"`
	Attachments            []CreateSessionAttachmentRef `json:"attachments,omitempty"`
}

type CreateSessionAttachmentRef struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
	ByteSize int64  `json:"byte_size,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	BlobRef  string `json:"blob_ref,omitempty"`
}

// SessionCreatedPayload 新代理会话创建结果（电脑→手机）
type SessionCreatedPayload struct {
	RequestID      string `json:"request_id,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	Error          string `json:"error,omitempty"`
}

// SessionActionPayload 会话管理动作（手机→电脑）
type SessionActionPayload struct {
	RequestID string `json:"request_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Action    string `json:"action,omitempty"` // interrupt / pause / stop / delete
}

// SessionActionResultPayload 会话管理动作结果（电脑→手机）
type SessionActionResultPayload struct {
	RequestID      string `json:"request_id,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	Action         string `json:"action,omitempty"`
	LifecycleState string `json:"lifecycle_state,omitempty"`
	OccurredAt     int64  `json:"occurred_at,omitempty"`
	Error          string `json:"error,omitempty"`
}

// SessionLifecyclePayload 会话生命周期事件（电脑→手机）
type SessionLifecyclePayload struct {
	State         string `json:"state,omitempty"`
	PreviousState string `json:"previous_state,omitempty"`
	Action        string `json:"action,omitempty"`
	RequestID     string `json:"request_id,omitempty"`
	Reason        string `json:"reason,omitempty"`
	OccurredAt    int64  `json:"occurred_at,omitempty"`
}

// ListDirPayload 目录浏览请求（手机→电脑）
type ListDirPayload struct {
	Path      string `json:"path"`                 // 要列出的目录路径
	Query     string `json:"query,omitempty"`      // 可选搜索词
	Limit     int    `json:"limit,omitempty"`      // 最大返回条目数
	Recursive bool   `json:"recursive,omitempty"`  // 是否递归搜索
	RequestID string `json:"request_id,omitempty"` // 请求 ID，用于匹配响应
}

// DirEntry 目录条目
type DirEntry struct {
	Name        string `json:"name"`
	Path        string `json:"path,omitempty"`         // 绝对路径
	DisplayPath string `json:"display_path,omitempty"` // 展示/插入路径
	IsDir       bool   `json:"is_dir"`
}

// ListDirResponsePayload 目录浏览响应（电脑→手机）
type ListDirResponsePayload struct {
	Path      string     `json:"path"`                 // 请求的目录路径
	RequestID string     `json:"request_id,omitempty"` // 对应的请求 ID
	Entries   []DirEntry `json:"entries"`              // 目录内容
	Error     string     `json:"error,omitempty"`      // 错误信息
}

// ReadFilePayload 文件预览请求（手机→电脑）
type ReadFilePayload struct {
	Path      string `json:"path"`
	RequestID string `json:"request_id,omitempty"`
}

// ReadFileResponsePayload 文件预览响应（电脑→手机）
type ReadFileResponsePayload struct {
	Path        string `json:"path"`
	RequestID   string `json:"request_id,omitempty"`
	Content     string `json:"content,omitempty"`
	Language    string `json:"language,omitempty"`
	SizeBytes   int64  `json:"size_bytes,omitempty"`
	IsTruncated bool   `json:"is_truncated,omitempty"`
	IsBinary    bool   `json:"is_binary,omitempty"`
	Error       string `json:"error,omitempty"`
}

// GitStatusPayload Git 仓库状态请求
type GitStatusPayload struct {
	Path      string `json:"path"`
	RequestID string `json:"request_id,omitempty"`
}

// GitChangedFile Git 变更文件
type GitChangedFile struct {
	Path           string `json:"path"`
	AbsolutePath   string `json:"absolute_path,omitempty"`
	PreviousPath   string `json:"previous_path,omitempty"`
	StagedStatus   string `json:"staged_status,omitempty"`
	UnstagedStatus string `json:"unstaged_status,omitempty"`
	IsUntracked    bool   `json:"is_untracked,omitempty"`
	IsConflicted   bool   `json:"is_conflicted,omitempty"`
}

// GitStatusResponsePayload Git 仓库状态响应
type GitStatusResponsePayload struct {
	Path               string           `json:"path"`
	RequestID          string           `json:"request_id,omitempty"`
	RepoRoot           string           `json:"repo_root,omitempty"`
	RepoName           string           `json:"repo_name,omitempty"`
	Branch             string           `json:"branch,omitempty"`
	HeadOID            string           `json:"head_oid,omitempty"`
	Upstream           string           `json:"upstream,omitempty"`
	AheadCount         int              `json:"ahead_count,omitempty"`
	BehindCount        int              `json:"behind_count,omitempty"`
	IsDetached         bool             `json:"is_detached,omitempty"`
	HasStagedChanges   bool             `json:"has_staged_changes,omitempty"`
	HasUnstagedChanges bool             `json:"has_unstaged_changes,omitempty"`
	Files              []GitChangedFile `json:"files,omitempty"`
	Error              string           `json:"error,omitempty"`
}

// GitDiffPayload Git diff 请求
type GitDiffPayload struct {
	Path      string `json:"path"`
	FilePath  string `json:"file_path,omitempty"`
	Staged    bool   `json:"staged,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// GitDiffResponsePayload Git diff 响应
type GitDiffResponsePayload struct {
	Path        string `json:"path"`
	FilePath    string `json:"file_path,omitempty"`
	RequestID   string `json:"request_id,omitempty"`
	RepoRoot    string `json:"repo_root,omitempty"`
	Staged      bool   `json:"staged,omitempty"`
	Diff        string `json:"diff,omitempty"`
	IsTruncated bool   `json:"is_truncated,omitempty"`
	Error       string `json:"error,omitempty"`
}

// GitLogPayload Git 提交记录请求
type GitLogPayload struct {
	Path      string `json:"path"`
	Limit     int    `json:"limit,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// GitCommitLogEntry Git 提交记录条目
type GitCommitLogEntry struct {
	OID         string   `json:"oid"`
	ShortOID    string   `json:"short_oid,omitempty"`
	Subject     string   `json:"subject,omitempty"`
	AuthorName  string   `json:"author_name,omitempty"`
	AuthorEmail string   `json:"author_email,omitempty"`
	AuthoredAt  string   `json:"authored_at,omitempty"`
	ParentOIDs  []string `json:"parent_oids,omitempty"`
}

// GitLogResponsePayload Git 提交记录响应
type GitLogResponsePayload struct {
	Path      string              `json:"path"`
	RequestID string              `json:"request_id,omitempty"`
	RepoRoot  string              `json:"repo_root,omitempty"`
	RepoName  string              `json:"repo_name,omitempty"`
	Branch    string              `json:"branch,omitempty"`
	Commits   []GitCommitLogEntry `json:"commits,omitempty"`
	Error     string              `json:"error,omitempty"`
}

// GitCommitDetailPayload Git 提交详情请求
type GitCommitDetailPayload struct {
	Path      string `json:"path"`
	Commit    string `json:"commit"`
	RequestID string `json:"request_id,omitempty"`
}

// GitCommitFileStat Git 提交中的文件统计
type GitCommitFileStat struct {
	Path      string `json:"path"`
	Additions int    `json:"additions,omitempty"`
	Deletions int    `json:"deletions,omitempty"`
	IsBinary  bool   `json:"is_binary,omitempty"`
}

// GitCommitDetailResponsePayload Git 提交详情响应
type GitCommitDetailResponsePayload struct {
	Path        string              `json:"path"`
	RequestID   string              `json:"request_id,omitempty"`
	RepoRoot    string              `json:"repo_root,omitempty"`
	RepoName    string              `json:"repo_name,omitempty"`
	Commit      string              `json:"commit,omitempty"`
	ShortOID    string              `json:"short_oid,omitempty"`
	Subject     string              `json:"subject,omitempty"`
	Body        string              `json:"body,omitempty"`
	AuthorName  string              `json:"author_name,omitempty"`
	AuthorEmail string              `json:"author_email,omitempty"`
	AuthoredAt  string              `json:"authored_at,omitempty"`
	ParentOIDs  []string            `json:"parent_oids,omitempty"`
	Files       []GitCommitFileStat `json:"files,omitempty"`
	Diff        string              `json:"diff,omitempty"`
	IsTruncated bool                `json:"is_truncated,omitempty"`
	Error       string              `json:"error,omitempty"`
}

// ListCommandsPayload Slash 命令列表请求（手机→电脑）
type ListCommandsPayload struct {
	Query     string `json:"query,omitempty"`      // 当前输入的命令前缀，不含 /
	RequestID string `json:"request_id,omitempty"` // 请求 ID，用于匹配响应
}

// SlashCommandEntry Slash 命令条目
type SlashCommandEntry struct {
	Name         string `json:"name"`
	Summary      string `json:"summary,omitempty"`
	Source       string `json:"source,omitempty"`        // built-in / global / project
	ArgumentHint string `json:"argument_hint,omitempty"` // 参数提示
}

// ListCommandsResponsePayload Slash 命令列表响应（电脑→手机）
type ListCommandsResponsePayload struct {
	Query     string              `json:"query,omitempty"`      // 原始查询前缀
	RequestID string              `json:"request_id,omitempty"` // 对应的请求 ID
	Commands  []SlashCommandEntry `json:"commands"`             // 命令列表
	Error     string              `json:"error,omitempty"`      // 错误信息
}

// ListSkillsPayload Codex 技能列表请求（手机→电脑）
type ListSkillsPayload struct {
	Query     string `json:"query,omitempty"`      // 当前输入的技能前缀，不含 $
	RequestID string `json:"request_id,omitempty"` // 请求 ID，用于匹配响应
}

// SkillEntry Codex 技能条目
type SkillEntry struct {
	Name    string `json:"name"`
	Summary string `json:"summary,omitempty"`
	Source  string `json:"source,omitempty"` // user / shared / system
	Path    string `json:"path,omitempty"`
}

// ListSkillsResponsePayload Codex 技能列表响应（电脑→手机）
type ListSkillsResponsePayload struct {
	Query     string       `json:"query,omitempty"`      // 原始查询前缀
	RequestID string       `json:"request_id,omitempty"` // 对应的请求 ID
	Skills    []SkillEntry `json:"skills"`               // 技能列表
	Error     string       `json:"error,omitempty"`      // 错误信息
}

// ProtectedBlob 通用保护载荷
type ProtectedBlob struct {
	Alg        string            `json:"alg,omitempty"`
	Scope      string            `json:"scope,omitempty"`
	KeyVersion int               `json:"key_version,omitempty"`
	Nonce      string            `json:"nonce,omitempty"`
	Ciphertext string            `json:"ciphertext,omitempty"`
	Meta       map[string]string `json:"meta,omitempty"`
}

// SessionKeyRequestPayload 会话内容密钥请求（手机→电脑）
type SessionKeyRequestPayload struct {
	RequestID       string `json:"request_id,omitempty"`
	ClientPublicKey string `json:"client_public_key"`
	Scope           string `json:"scope,omitempty"`
}

// SessionKeyResponsePayload 会话内容密钥响应（电脑→手机）
type SessionKeyResponsePayload struct {
	RequestID     string         `json:"request_id,omitempty"`
	HostPublicKey string         `json:"host_public_key,omitempty"`
	Scope         string         `json:"scope,omitempty"`
	WrappedKey    *ProtectedBlob `json:"wrapped_key,omitempty"`
	Error         string         `json:"error,omitempty"`
}

// SessionHistoryMessage 远程会话历史消息
type SessionHistoryMessage struct {
	SessionID        string `json:"session_id,omitempty"`
	RuntimeSessionID string `json:"runtime_session_id,omitempty"`
	SourceID         string `json:"source_id,omitempty"`
	SourceKind       string `json:"source_kind,omitempty"`
	Role             string `json:"role"`
	Content          string `json:"content"`
	MessageType      string `json:"message_type,omitempty"`
	ToolName         string `json:"tool_name,omitempty"`
	ToolCallID       string `json:"tool_call_id,omitempty"`
	ToolInput        string `json:"tool_input,omitempty"`
	ToolResult       string `json:"tool_result,omitempty"`
	IsToolComplete   bool   `json:"is_tool_complete,omitempty"`
	Timestamp        int64  `json:"timestamp"`
}

// SessionHistoryPayload 会话历史响应
type SessionHistoryPayload struct {
	SessionID        string                  `json:"session_id"`
	RuntimeSessionID string                  `json:"runtime_session_id,omitempty"`
	Messages         []SessionHistoryMessage `json:"messages"`
	Offset           int                     `json:"offset,omitempty"`
	NextOffset       int                     `json:"next_offset,omitempty"`
	HasMore          bool                    `json:"has_more,omitempty"`
	Total            int                     `json:"total,omitempty"`
}

// SessionHistorySyncRequest 会话历史同步请求
type SessionHistorySyncRequest struct {
	SessionID        string                  `json:"session_id"`
	RuntimeSessionID string                  `json:"runtime_session_id,omitempty"`
	Messages         []SessionHistoryMessage `json:"messages"`
}
