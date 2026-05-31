// Package persist handles persisting session state.
const std = @import("std");
const Allocator = std.mem.Allocator;
const store = @import("../store/store.zig");

/// persistDead persists a dead session.
pub fn persistDead(allocator: Allocator, session: *store.Session) !void {
    _ = allocator;
    _ = session;
    // In real implementation, this would write to disk
}
