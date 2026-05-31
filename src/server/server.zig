// Package server provides the HTTP server for gmuxd.
const std = @import("std");
const httpz = @import("httpz");
const Allocator = std.mem.Allocator;

/// Server wraps httpz.Server for gmuxd.
pub const Server = struct {
    server: *httpz.Server,
    allocator: Allocator,
    port: u16,
    bind: []const u8,

    pub fn init(allocator: Allocator, port: u16, bind: []const u8) !*Server {
        const self = try allocator.create(Server);
        errdefer allocator.destroy(self);

        self.allocator = allocator;
        self.port = port;
        self.bind = try allocator.dupe(u8, bind);

        var config = httpz.Config{};
        config.port = port;
        config.bind = bind;
        config.thread_pool_size = 4;

        self.server = try httpz.Server.init(allocator, config);

        return self;
    }

    pub fn deinit(self: *Server) void {
        self.allocator.free(self.bind);
        self.server.deinit();
        self.allocator.destroy(self);
    }

    /// Get registers a GET handler.
    pub fn get(self: *Server, path: []const u8, handler: anytype) !void {
        try self.server.router.get(path, handler);
    }

    /// Post registers a POST handler.
    pub fn post(self: *Server, path: []const u8, handler: anytype) !void {
        try self.server.router.post(path, handler);
    }

    /// Put registers a PUT handler.
    pub fn put(self: *Server, path: []const u8, handler: anytype) !void {
        try self.server.router.put(path, handler);
    }

    /// Delete registers a DELETE handler.
    pub fn delete(self: *Server, path: []const u8, handler: anytype) !void {
        try self.server.router.delete(path, handler);
    }

    /// Patch registers a PATCH handler.
    pub fn patch(self: *Server, path: []const u8, handler: anytype) !void {
        try self.server.router.patch(path, handler);
    }

    /// Start begins serving HTTP requests.
    pub fn start(self: *Server) !void {
        try self.server.start();
    }

    /// Stop shuts down the server.
    pub fn stop(self: *Server) void {
        self.server.stop();
    }
};
