// Package testutil provides test utilities.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// TestUtil provides test utilities.
pub const TestUtil = struct {
    allocator: Allocator,

    pub fn init(allocator: Allocator) !*TestUtil {
        const self = try allocator.create(TestUtil);
        errdefer allocator.destroy(self);

        self.allocator = allocator;

        return self;
    }

    pub fn deinit(self: *TestUtil) void {
        self.allocator.destroy(self);
    }
};
