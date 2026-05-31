// Package testutil provides test utilities.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// Harness is a test harness.
pub const Harness = struct {
    allocator: Allocator,

    pub fn init(allocator: Allocator) !*Harness {
        const self = try allocator.create(Harness);
        errdefer allocator.destroy(self);

        self.allocator = allocator;

        return self;
    }

    pub fn deinit(self: *Harness) void {
        self.allocator.destroy(self);
    }
};
