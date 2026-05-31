// Package sseclient provides an SSE client for subscribing to events.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// Client is an SSE client.
pub const Client = struct {
    allocator: Allocator,
    url: []const u8,
    lastEventId: []const u8,

    pub fn init(allocator: Allocator, url: []const u8) !*Client {
        const self = try allocator.create(Client);
        errdefer allocator.destroy(self);

        self.allocator = allocator;
        self.url = try allocator.dupe(u8, url);
        self.lastEventId = try allocator.dupe(u8, "");

        return self;
    }

    pub fn deinit(self: *Client) void {
        self.allocator.free(self.url);
        self.allocator.free(self.lastEventId);
        self.allocator.destroy(self);
    }

    /// subscribe starts an SSE subscription.
    pub fn subscribe(self: *Client, handler: anytype) !void {
        _ = self;
        _ = handler;
        // In real implementation, this would connect to the SSE endpoint
    }
};
