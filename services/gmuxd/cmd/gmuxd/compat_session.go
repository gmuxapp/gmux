package main

// compatSession is an authority-neutral rendering/resume DTO retained at the
// HTTP boundary. It deliberately has no store behavior.
type compatSession struct {
	ID, Peer, CreatedAt, Cwd, Adapter, WorkspaceRoot, ParentSessionID string
	StartedAt, ExitedAt, Title, Subtitle, LastOutputAt              string
	SocketPath, Slug, ConversationRef, RunnerVersion, BinaryHash      string
	ShellTitle, AdapterTitle, ProjectSlug                             string
	Command                                                           []string
	Remotes                                                           map[string]string
	Alive, Unread, Resumable                                          bool
	Pid, ProjectIndex                                                 int
	ExitCode                                                          *int
	Status                                                            *compatStatus
	TerminalCols, TerminalRows                                        uint16
}

type compatStatus struct{ Working, Error bool }
