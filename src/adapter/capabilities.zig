// Package adapter defines the adapter interface and related types.
const std = @import("std");

/// SessionFileInfo holds metadata extracted from a tool's session file.
pub const SessionFileInfo = struct {
    id: []const u8,
    title: []const u8,
    slug: []const u8,
    cwd: []const u8,
    created: i64,
    messageCount: usize,
    filePath: []const u8,
};

/// Event is a partial session state update emitted by an adapter.
pub const Event = struct {
    title: ?[]const u8 = null,
    status: ?Status = null,
    unread: ?bool = null,
    cwd: ?[]const u8 = null,
};

/// Status represents the session status.
pub const Status = enum {
    starting,
    running,
    idle,
    finished,
    err_state,
};

/// AdapterBase is the base struct for all adapters.
pub const AdapterBase = struct {
    name: []const u8,
};

/// Adapter is the interface for all adapters.
pub const Adapter = struct {
    /// name returns the adapter name.
    pub fn name(self: *const AdapterBase) []const u8 {
        return self.name;
    }
};

/// Launcher is a launch preset.
pub const Launcher = struct {
    name: []const u8,
    command: []const []const u8,
};
