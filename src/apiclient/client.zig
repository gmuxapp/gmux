// Package apiclient provides the API client.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// Client is the API client.
pub const Client = struct {
    allocator: Allocator,
    baseUrl: []const u8,
    token: []const u8,

    pub fn init(allocator: Allocator, baseUrl: []const u8, token: []const u8) !*Client {
        const self = try allocator.create(Client);
        errdefer allocator.destroy(self);

        self.allocator = allocator;
        self.baseUrl = try allocator.dupe(u8, baseUrl);
        self.token = try allocator.dupe(u8, token);

        return self;
    }

    pub fn deinit(self: *Client) void {
        self.allocator.free(self.baseUrl);
        self.allocator.free(self.token);
        self.allocator.destroy(self);
    }

    /// get performs a GET request.
    pub fn get(self: *Client, path: []const u8) ![]const u8 {
        _ = self;
        _ = path;
        return "";
    }

    /// post performs a POST request.
    pub fn post(self: *Client, path: []const u8, body: []const u8) ![]const u8 {
        _ = self;
        _ = path;
        _ = body;
        return "";
    }
};
