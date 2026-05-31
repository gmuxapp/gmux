// Package netauth handles network authentication.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// Middleware validates auth tokens on incoming requests.
pub const Middleware = struct {
    allocator: Allocator,
    token: []const u8,

    pub fn init(allocator: Allocator, token: []const u8) !*Middleware {
        const self = try allocator.create(Middleware);
        errdefer allocator.destroy(self);

        self.allocator = allocator;
        self.token = try allocator.dupe(u8, token);

        return self;
    }

    pub fn deinit(self: *Middleware) void {
        self.allocator.free(self.token);
        self.allocator.destroy(self);
    }

    /// validate validates an auth token.
    pub fn validate(self: *Middleware, provided: []const u8) bool {
        return std.mem.eql(u8, self.token, provided);
    }
};

test "netauth validate correct token" {
    var m = try Middleware.init(std.testing.allocator, "secret-token");
    defer m.deinit();

    try std.testing.expect(m.validate("secret-token"));
    try std.testing.expect(!m.validate("wrong-token"));
    try std.testing.expect(!m.validate(""));
}
