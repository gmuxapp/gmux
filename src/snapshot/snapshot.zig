// Package snapshot composes the wire payloads for the SSE stream.
const std = @import("std");
const Allocator = std.mem.Allocator;
const store = @import("../store/store.zig");

/// SessionsPayload is the body of a snapshot.sessions SSE event.
pub const SessionsPayload = struct {
    sessions: []const store.Session,
};

/// WorldPayload is the body of a snapshot.world SSE event.
pub const WorldPayload = struct {
    projects: []const u8,
    peers: []const u8,
    health: []const u8,
    launchers: []const u8,
    defaultLauncher: []const u8,
    peerProjects: ?[]const u8 = null,
};

/// ComposeSessions builds a snapshot.sessions payload from the live store.
pub fn composeSessions(allocator: Allocator, sessions: []const *store.Session) !SessionsPayload {
    var result = std.ArrayList(store.Session).init(allocator);
    errdefer result.deinit();

    for (sessions) |sess| {
        try result.append(sess.*);
    }

    return SessionsPayload{
        .sessions = try result.toOwnedSlice(),
    };
}

/// ComposeWorld builds a snapshot.world payload.
pub fn composeWorld(
    allocator: Allocator,
    projects: []const u8,
    peers: []const u8,
    health: []const u8,
    launchers: []const u8,
    defaultLauncher: []const u8,
) !WorldPayload {
    return WorldPayload{
        .projects = try allocator.dupe(u8, projects),
        .peers = try allocator.dupe(u8, peers),
        .health = try allocator.dupe(u8, health),
        .launchers = try allocator.dupe(u8, launchers),
        .defaultLauncher = try allocator.dupe(u8, defaultLauncher),
    };
}

test "composeWorld" {
    const a = std.testing.allocator;
    const w = try composeWorld(a, "proj", "peer", "health", "launcher", "default");
    defer {
        a.free(w.projects);
        a.free(w.peers);
        a.free(w.health);
        a.free(w.launchers);
        a.free(w.defaultLauncher);
    }

    try std.testing.expectEqualStrings("proj", w.projects);
    try std.testing.expectEqualStrings("peer", w.peers);
    try std.testing.expectEqualStrings("health", w.health);
    try std.testing.expectEqualStrings("launcher", w.launchers);
    try std.testing.expectEqualStrings("default", w.defaultLauncher);
    try std.testing.expect(w.peerProjects == null);
}
