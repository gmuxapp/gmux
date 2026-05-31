// Package claude provides the Claude Code adapter.
const std = @import("std");
const Allocator = std.mem.Allocator;
const adapter = @import("../adapter/adapter.zig");

/// Claude is the Claude Code adapter.
pub const Claude = struct {
    base: adapter.AdapterBase,
    allocator: Allocator,

    pub fn init(allocator: Allocator) !*Claude {
        const self = try allocator.create(Claude);
        errdefer allocator.destroy(self);

        self.base = .{ .name = "claude" };
        self.allocator = allocator;

        return self;
    }

    pub fn deinit(self: *Claude) void {
        self.allocator.destroy(self);
    }
};
