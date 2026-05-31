// Package update checks for updates.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// Checker checks for updates.
pub const Checker = struct {
    allocator: Allocator,
    currentVersion: []const u8,
    availableVersion: ?[]const u8,

    pub fn init(allocator: Allocator, currentVersion: []const u8) !*Checker {
        const self = try allocator.create(Checker);
        errdefer allocator.destroy(self);

        self.allocator = allocator;
        self.currentVersion = try allocator.dupe(u8, currentVersion);
        self.availableVersion = null;

        return self;
    }

    pub fn deinit(self: *Checker) void {
        self.allocator.free(self.currentVersion);
        if (self.availableVersion) |v| {
            self.allocator.free(v);
        }
        self.allocator.destroy(self);
    }

    /// check checks for updates.
    pub fn check(self: *Checker) !void {
        _ = self;
        // In real implementation, this would check for updates
    }

    /// available returns the available version if any.
    pub fn available(self: *Checker) ?[]const u8 {
        return self.availableVersion;
    }
};
