// Package pi provides the Pi adapter.
const std = @import("std");
const Allocator = std.mem.Allocator;
const adapter = @import("../adapter/adapter.zig");

/// Pi is the Pi adapter.
pub const Pi = struct {
    base: adapter.AdapterBase,
    allocator: Allocator,

    pub fn init(allocator: Allocator) !*Pi {
        const self = try allocator.create(Pi);
        errdefer allocator.destroy(self);

        self.base = .{ .name = "pi" };
        self.allocator = allocator;

        return self;
    }

    pub fn deinit(self: *Pi) void {
        self.allocator.destroy(self);
    }
};
