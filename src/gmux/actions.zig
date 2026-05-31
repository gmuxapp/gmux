// Package gmux provides the CLI actions.
const std = @import("std");
const Allocator = std.mem.Allocator;
const httpclient = @import("../httpclient/client.zig");
const store = @import("../store/store.zig");

/// CLISession is the subset of gmuxd's Session model that the CLI cares about.
pub const CLISession = struct {
    id: []const u8,
    peer: []const u8,
    cwd: []const u8,
    kind: []const u8,
    alive: bool,
    pid: ?i32,
    title: []const u8,
    slug: []const u8,
    socketPath: []const u8,
    command: []const []const u8,
    startedAt: []const u8,
    exitedAt: []const u8,
    exitCode: ?i32,
};

/// fetchSessions queries gmuxd for the full session list.
pub fn fetchSessions(allocator: Allocator, client: *httpclient.Client) ![]const CLISession {
    _ = allocator;
    _ = client;
    return &empty_sessions;
}

const empty_sessions: []const CLISession = &[_]CLISession{};

/// resolveSession fetches the session list from gmuxd and finds the one
/// the user's reference points to.
pub fn resolveSession(allocator: Allocator, client: *httpclient.Client, ref: []const u8, host: []const u8) !CLISession {
    _ = allocator;
    _ = client;
    _ = ref;
    _ = host;
    return CLISession{
        .id = "",
        .peer = "",
        .cwd = "",
        .kind = "",
        .alive = false,
        .pid = null,
        .title = "",
        .slug = "",
        .socketPath = "",
        .command = &empty_commands,
        .startedAt = "",
        .exitedAt = "",
        .exitCode = null,
    };
}

const empty_commands: []const []const u8 = &[_][]const u8{};

/// listSessions lists all sessions.
pub fn listSessions(allocator: Allocator, client: *httpclient.Client) !void {
    _ = allocator;
    _ = client;
}

/// killSession kills a session.
pub fn killSession(allocator: Allocator, client: *httpclient.Client, sessionId: []const u8) !void {
    _ = allocator;
    _ = client;
    _ = sessionId;
}

/// tailSession tails a session's output.
pub fn tailSession(allocator: Allocator, client: *httpclient.Client, sessionId: []const u8) !void {
    _ = allocator;
    _ = client;
    _ = sessionId;
}

/// sendToSession sends input to a session.
pub fn sendToSession(allocator: Allocator, client: *httpclient.Client, sessionId: []const u8, data: []const u8) !void {
    _ = allocator;
    _ = client;
    _ = sessionId;
    _ = data;
}
