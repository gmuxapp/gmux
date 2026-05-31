// Package scrollback persists a session's raw PTY output stream so a
// dead session can still serve its terminal history.
//
// File layout under the per-session state dir:
//   scrollback     - append-only active file, capped at MaxBytes
//   scrollback.0   - previous active file (rotated)
const std = @import("std");
const Allocator = std.mem.Allocator;
const File = std.fs.File;

/// ActiveName is the basename of the active scrollback file.
pub const ActiveName = "scrollback";

/// PreviousName is the basename of the rotated previous file.
pub const PreviousName = "scrollback.0";

/// MaxBytes is the soft cap at which the active file is rotated (1 MiB).
pub const MaxBytes: i64 = 1 << 20;

/// dirMode and fileMode for per-session directory and files.
pub const dirMode: u32 = 0o700;
pub const fileMode: u32 = 0o600;

/// Writer appends raw PTY bytes to the active scrollback file with
/// size-based rotation.
pub const Writer = struct {
    path: []u8,
    max: i64,
    file: ?File,
    written: i64,
    failErr: ?anyerror,
    closed: bool,
    allocator: Allocator,

    /// Open creates or opens the active scrollback file at path.
    pub fn open(path: []const u8, allocator: Allocator) !Writer {
        // Create parent directory
        const dir = std.fs.path.dirname(path) orelse return error.InvalidPath;
        std.fs.cwd().makePath(dir) catch {};

        // Remove any previous rotated file
        const prevPath = try std.fs.path.join(allocator, &.{ dir, PreviousName });
        defer allocator.free(prevPath);
        std.fs.deleteFileAbsolute(prevPath) catch {};

        const file = try File.openFileUpdate(path);
        // Truncate existing file
        try file.setEndOfFile(0);

        return Writer{
            .path = try allocator.dupe(u8, path),
            .max = MaxBytes,
            .file = file,
            .written = 0,
            .failErr = null,
            .closed = false,
            .allocator = allocator,
        };
    }

    /// Write appends p to the active file, rotating first if needed.
    /// Returns len(p) even on IO failure (best-effort).
    pub fn write(self: *Writer, p: []const u8) usize {
        if (self.closed or self.failErr != null or p.len == 0) {
            return p.len;
        }

        if (self.written + @as(i64, @intCast(p.len)) > self.max) {
            if (self.rotate() == null) {
                // Rotation failed, set error and return
                return p.len;
            }
        }

        if (self.file) |*f| {
            f.writeAll(p) catch |err| {
                self.failErr = err;
            };
            self.written += @as(i64, @intCast(p.len));
        }

        return p.len;
    }

    /// rotate closes the active file, renames it to PreviousName,
    /// and opens a fresh active file. Returns true on success.
    fn rotate(self: *Writer) ?void {
        if (self.file) |f| {
            f.sync() catch {};
            f.close();
        }

        const dir = std.fs.path.dirname(self.path) orelse return null;
        const prevPath = std.fs.path.join(self.allocator, &.{ dir, PreviousName }) catch return null;
        defer self.allocator.free(prevPath);

        std.fs.renameFileAbsolute(self.path, prevPath) catch {
            self.failErr = error.RotateFailed;
            return null;
        };

        const newFile = File.openFileUpdate(self.path) catch {
            self.failErr = error.ReopenFailed;
            return null;
        };
        newFile.setEndOfFile(0) catch {
            self.failErr = error.TruncateFailed;
            return null;
        };
        self.file = newFile;
        self.written = 0;
        return {};
    }

    /// Close flushes and closes the active file. Idempotent.
    pub fn close(self: *Writer) void {
        if (self.closed) {
            return;
        }
        self.closed = true;

        if (self.file) |f| {
            f.sync() catch |err| {
                if (self.failErr == null) {
                    self.failErr = err;
                }
            };
            f.close();
        }
        self.file = null;
    }

    pub fn deinit(self: *Writer) void {
        self.close();
        if (self.file) |f| {
            f.close();
        }
        self.allocator.free(self.path);
    }
};

/// ScrollbackReader reads persisted scrollback data.
/// Reads previous file first, then active file (chronological order).
pub const ScrollbackReader = struct {
    prev_file: ?File,
    active_file: ?File,
    prev_exhausted: bool,
    allocator: Allocator,

    pub fn open(dir: []const u8, allocator: Allocator) !ScrollbackReader {
        const prevPath = try std.fs.path.join(allocator, &.{ dir, PreviousName });
        defer allocator.free(prevPath);

        const activePath = try std.fs.path.join(allocator, &.{ dir, ActiveName });
        defer allocator.free(activePath);

        var prev_file: ?File = null;
        var active_file: ?File = null;

        const prev = File.openFile(prevPath) catch |err| {
            if (err == error.FileNotFound) {
                null;
            } else {
                return err;
            }
        };
        prev_file = prev;

        const active = File.openFile(activePath) catch |err| {
            if (err == error.FileNotFound) {
                if (prev_file == null) {
                    return error.FileNotFound;
                }
                null;
            } else {
                if (prev_file) |f| f.close();
                return err;
            }
        };
        active_file = active;

        if (prev_file == null and active_file == null) {
            return error.FileNotFound;
        }

        return ScrollbackReader{
            .prev_file = prev_file,
            .active_file = active_file,
            .prev_exhausted = false,
            .allocator = allocator,
        };
    }

    pub fn read(self: *ScrollbackReader, buf: []u8) !usize {
        if (self.prev_file) |*f| {
            if (!self.prev_exhausted) {
                const result = f.read(buf);
                switch (result) {
                    error.EndOfFile => {
                        self.prev_exhausted = true;
                        f.close();
                        self.prev_file = null;
                    },
                    else => |val| return val,
                }
            }
        }

        if (self.active_file) |*f| {
            return f.read(buf);
        }

        return error.EndOfFile;
    }

    pub fn close(self: *ScrollbackReader) void {
        if (self.prev_file) |f| f.close();
        if (self.active_file) |f| f.close();
    }

    pub fn deinit(self: *ScrollbackReader) void {
        self.close();
    }
};

/// RenderScrollbackSize is the scrollback ring kept by the virtual terminal.
pub const RenderScrollbackSize = 2000;

test "writer and reader" {
    const allocator = testing.allocator;
    const tmpDir = "/tmp/gmux-scrollback-test";

    std.fs.deleteTreeAbsolute(tmpDir) catch {};
    try std.fs.makeDirAbsolute(tmpDir);
    defer std.fs.deleteTreeAbsolute(tmpDir) catch {};

    const scrollPath = try std.fs.path.join(allocator, &.{ tmpDir, ActiveName });
    defer allocator.free(scrollPath);

    var writer = try Writer.open(scrollPath, allocator);
    defer writer.deinit();

    const data = "Hello, World!\r\n";
    writer.write(data);
    writer.close();

    var reader = try ScrollbackReader.open(tmpDir, allocator);
    defer reader.deinit();

    var buf: [64]u8 = undefined;
    const n = reader.read(&buf) catch unreachable;
    try std.testing.expectEqual(@as(usize, data.len), n);
    try std.testing.expectEqualStrings(data, buf[0..n]);
}

test "writer rotation" {
    const allocator = testing.allocator;
    const tmpDir = "/tmp/gmux-scrollback-rotate";

    std.fs.deleteTreeAbsolute(tmpDir) catch {};
    try std.fs.makeDirAbsolute(tmpDir);
    defer std.fs.deleteTreeAbsolute(tmpDir) catch {};

    const scrollPath = try std.fs.path.join(allocator, &.{ tmpDir, ActiveName });
    defer allocator.free(scrollPath);

    var writer = try Writer.open(scrollPath, allocator);
    writer.max = 20; // Small cap for testing
    defer writer.deinit();

    // Write enough to trigger rotation
    writer.write("0123456789");
    writer.write("0123456789");
    writer.write("0123456789"); // This should trigger rotation
    writer.close();

    // Check that previous file exists
    const prevPath = try std.fs.path.join(allocator, &.{ tmpDir, PreviousName });
    defer allocator.free(prevPath);
    const prevStat = std.fs.statFileAbsolute(prevPath);
    try std.testing.expect(prevStat != null);
}

const testing = std.testing;
