// Package gmux provides the attach command.
const std = @import("std");
const Allocator = std.mem.Allocator;
const localterm = @import("../localterm/localterm.zig");
const ptyserver = @import("../ptyserver/ptyserver.zig");

/// attach attaches to a local session.
pub fn attach(allocator: Allocator, sessionId: []const u8) !void {
    _ = allocator;
    _ = sessionId;
    // In real implementation, this would attach to the session
}
