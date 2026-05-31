// Package codex provides the Codex CLI adapter.
const std = @import("std");
const Allocator = std.mem.Allocator;
const adapter = @import("../adapter/adapter.zig");

/// Codex is the Codex CLI adapter.
pub const Codex = struct {
    base: adapter.AdapterBase,
    allocator: Allocator,

    pub fn init(allocator: Allocator) !*Codex {
        const self = try allocator.create(Codex);
        errdefer allocator.destroy(self);

        self.base = .{ .name = "codex" };
        self.allocator = allocator;

        return self;
    }

    pub fn deinit(self: *Codex) void {
        self.allocator.destroy(self);
    }
};
