// Package clipfile handles clipboard file operations.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// Writer writes clipboard data to a file.
pub const Writer = struct {
    allocator: Allocator,
    dir: []const u8,

    pub fn init(allocator: Allocator, dir: []const u8) !*Writer {
        const self = try allocator.create(Writer);
        errdefer allocator.destroy(self);

        self.allocator = allocator;
        self.dir = try allocator.dupe(u8, dir);

        return self;
    }

    pub fn deinit(self: *Writer) void {
        self.allocator.free(self.dir);
        self.allocator.destroy(self);
    }

    /// write writes clipboard data to a file.
    pub fn write(self: *Writer, data: []const u8) ![]const u8 {
        const filename = try std.fmt.allocPrint(self.allocator, "clipboard-{d}", .{std.time.milliTimestamp()});
        defer self.allocator.free(filename);

        const path = try std.fs.path.join(self.allocator, &.{ self.dir, filename });
        defer self.allocator.free(path);

        const file = try std.fs.createFileAbsolute(path, .{});
        defer file.close();

        try file.writeAll(data);

        return path;
    }
};

test "clipfile write and read" {
    const tmp_dir = std.fs.cwd().makeOpenPath(".test_clipfile", .{}) catch unreachable;
    defer tmp_dir.close();

    const dir_path = try std.fs.cwd().realpathAlloc(std.testing.allocator, ".test_clipfile");
    defer std.testing.allocator.free(dir_path);

    var w = try Writer.init(std.testing.allocator, dir_path);
    defer w.deinit();

    const path = try w.write("hello world");
    defer std.testing.allocator.free(path);

    const file = try std.fs.openFileAbsolute(path, .{});
    defer file.close();

    var buf: [20]u8 = undefined;
    const n = try file.readAll(&buf);
    try std.testing.expectEqualStrings("hello world", buf[0..n]);

    // Cleanup
    std.fs.cwd().deleteTree(".test_clipfile") catch {};
}
