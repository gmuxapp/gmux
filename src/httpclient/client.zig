// Package httpclient provides an HTTP client for communicating with gmuxd.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// Client is an HTTP client for gmuxd communication.
pub const Client = struct {
    allocator: Allocator,
    base_url: []const u8,
    auth_token: []const u8,

    pub fn init(allocator: Allocator, base_url: []const u8, auth_token: []const u8) !*Client {
        const self = try allocator.create(Client);
        errdefer allocator.destroy(self);

        self.allocator = allocator;
        self.base_url = try allocator.dupe(u8, base_url);
        self.auth_token = try allocator.dupe(u8, auth_token);

        return self;
    }

    pub fn deinit(self: *Client) void {
        self.allocator.free(self.base_url);
        self.allocator.free(self.auth_token);
        self.allocator.destroy(self);
    }

    /// get performs an HTTP GET request.
    pub fn get(self: *Client, path: []const u8) ![]const u8 {
        const url = try std.fs.path.join(self.allocator, &.{ self.base_url, path });
        defer self.allocator.free(url);

        // Use std.net to make the request
        var stream = try std.net.connect6(.{
            .address = std.net.Fmt.Any.init(.{ .ip4 = .{ .any = true } }).address(),
            .port = 8790,
        });
        defer stream.stream.close();

        const request = try std.fmt.allocPrint(self.allocator, "GET {s} HTTP/1.1\r\nHost: localhost\r\n\r\n", .{path});
        defer self.allocator.free(request);

        try stream.stream.writeAll(request);

        var buffer: [4096]u8 = undefined;
        const n = try stream.stream.read(&buffer);

        return self.allocator.dupe(u8, buffer[0..n]);
    }

    /// post performs an HTTP POST request.
    pub fn post(self: *Client, path: []const u8, body: []const u8) ![]const u8 {
        const url = try std.fs.path.join(self.allocator, &.{ self.base_url, path });
        defer self.allocator.free(url);

        var stream = try std.net.connect6(.{
            .address = std.net.Fmt.Any.init(.{ .ip4 = .{ .any = true } }).address(),
            .port = 8790,
        });
        defer stream.stream.close();

        const request = try std.fmt.allocPrint(self.allocator, "POST {s} HTTP/1.1\r\nHost: localhost\r\nContent-Length: {d}\r\n\r\n{s}", .{ path, body.len, body });
        defer self.allocator.free(request);

        try stream.stream.writeAll(request);

        var buffer: [4096]u8 = undefined;
        const n = try stream.stream.read(&buffer);

        return self.allocator.dupe(u8, buffer[0..n]);
    }
};
