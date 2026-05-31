// Package tsdiscovery discovers peers via Tailscale.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// Watcher watches for Tailscale peers.
pub const Watcher = struct {
    allocator: Allocator,

    pub fn init(allocator: Allocator) !*Watcher {
        const self = try allocator.create(Watcher);
        errdefer allocator.destroy(self);

        self.allocator = allocator;

        return self;
    }

    pub fn deinit(self: *Watcher) void {
        self.allocator.destroy(self);
    }

    /// discover discovers Tailscale peers.
    pub fn discover(self: *Watcher) !void {
        _ = self;
        // In real implementation, this would query the Tailscale API
    }
};
