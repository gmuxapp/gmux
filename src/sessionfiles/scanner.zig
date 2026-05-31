// Package sessionfiles scans for session files.
const std = @import("std");
const Allocator = std.mem.Allocator;
const store = @import("../store/store.zig");

/// Scanner scans for session files.
pub const Scanner = struct {
    allocator: Allocator,
    sessions: *store.Store,

    pub fn init(allocator: Allocator, sessions: *store.Store) !*Scanner {
        const self = try allocator.create(Scanner);
        errdefer allocator.destroy(self);

        self.allocator = allocator;
        self.sessions = sessions;

        return self;
    }

    pub fn deinit(self: *Scanner) void {
        self.allocator.destroy(self);
    }

    /// scan scans for session files.
    pub fn scan(self: *Scanner) !void {
        _ = self;
        // In real implementation, this would scan for session files
    }
};
