// Package wsproxy provides WebSocket proxying to runners.
const std = @import("std");
const Allocator = std.mem.Allocator;
const store = @import("../store/store.zig");

/// Proxy proxies WebSocket connections to runners.
pub const Proxy = struct {
    allocator: Allocator,
    sessions: *store.Store,

    pub fn init(allocator: Allocator, sessions: *store.Store) !*Proxy {
        const self = try allocator.create(Proxy);
        errdefer allocator.destroy(self);

        self.allocator = allocator;
        self.sessions = sessions;

        return self;
    }

    pub fn deinit(self: *Proxy) void {
        self.allocator.destroy(self);
    }

    /// handle handles a WebSocket upgrade request.
    pub fn handle(self: *Proxy, session_id: []const u8) !void {
        _ = self;
        _ = session_id;
        // In real implementation, this would proxy WebSocket to runner
    }
};
