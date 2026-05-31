// Package gmux provides the run command.
const std = @import("std");
const Allocator = std.mem.Allocator;
const session = @import("../session/state.zig");
const adapter = @import("../adapter/adapter.zig");
const naming = @import("../naming/naming.zig");
const metadata = @import("../metadata/metadata.zig");
const localterm = @import("../localterm/localterm.zig");
const ptyserver = @import("../ptyserver/ptyserver.zig");

/// run launches a new session.
pub fn run(allocator: Allocator, cmd: []const []const u8, cwd: []const u8) !void {
    _ = allocator;
    _ = cmd;
    _ = cwd;
    // In real implementation, this would launch a new session
}
