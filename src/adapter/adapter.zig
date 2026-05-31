// Package adapter defines the interface for teaching gmux how to work
// with specific tools. Adapters are matched by command and produce
// Status events for the sidebar.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// Status represents an application-reported status for the sidebar.
pub const Status = struct {
    /// Display text ("working", "3/5 passed")
    label: []const u8,
    /// True while adapter is busy (spinner, building)
    working: bool,
    /// True when the adapter hit a retryable error (red dot)
    error_: bool,

    pub fn init(label: []const u8, working: bool, error_: bool) Status {
        return .{
            .label = label,
            .working = working,
            .error_ = error_,
        };
    }
};

/// Event describes what changed (title, status, cwd).
pub const Event = struct {
    title: ?[]const u8 = null,
    status: ?Status = null,
    cwd: ?[]const u8 = null,
};

/// EnvContext provides launch context to Env().
pub const EnvContext = struct {
    cwd: []const u8,
    sessionId: []const u8,
    socketPath: []const u8,
};

/// Launcher describes how to start a new session with a given adapter.
pub const Launcher = struct {
    id: []const u8,
    label: []const u8,
    command: []const []const u8,
    description: ?[]const u8 = null,
    available: bool,
};

/// BaseName extracts the base name of a command argument, stripping path.
pub fn baseName(arg: []const u8) []const u8 {
    return std.fs.path.basename(arg);
}

/// Slugify converts a string to a URL-safe slug: lowercase, non-alphanum
/// runs replaced by hyphens, trimmed, capped at 40 characters.
pub fn slugify(allocator: Allocator, s: []const u8) ![]const u8 {
    const buf = try allocator.dupe(u8, s);
    defer allocator.free(buf);
    std.ascii.lowerString(buf);

    // First pass: count output size
    var out_len: usize = 0;
    var in_bad: bool = false;
    var i: usize = 0;
    while (i < buf.len) : (i += 1) {
        const c = buf[i];
        const is_good = (c >= 'a' and c <= 'z') or (c >= '0' and c <= '9');
        if (is_good) {
            if (in_bad and out_len > 0) {
                out_len += 1; // for hyphen
            }
            out_len += 1;
            in_bad = false;
        } else {
            in_bad = true;
        }
    }

    // Trim leading/trailing hyphens
    var start: usize = 0;
    while (start < buf.len and !isSlugChar(buf[start])) start += 1;
    var end: usize = buf.len;
    while (end > start and !isSlugChar(buf[end - 1])) end -= 1;

    if (start >= end) {
        return allocator.dupe(u8, "");
    }

    // Recalculate length after trimming
    out_len = 0;
    in_bad = false;
    i = start;
    while (i < end) : (i += 1) {
        const c = buf[i];
        const is_good = isSlugChar(c);
        if (is_good) {
            if (in_bad and out_len > 0) {
                out_len += 1;
            }
            out_len += 1;
            in_bad = false;
        } else {
            in_bad = true;
        }
    }

    // Cap at 40
    if (out_len > 40) out_len = 40;

    const result = try allocator.alloc(u8, out_len);
    errdefer allocator.free(result);

    // Second pass: build output
    var pos: usize = 0;
    in_bad = false;
    i = start;
    while (i < end and pos < out_len) : (i += 1) {
        const c = buf[i];
        const is_good = isSlugChar(c);
        if (is_good) {
            if (in_bad and pos > 0) {
                result[pos] = '-';
                pos += 1;
            }
            if (pos < out_len) {
                result[pos] = c;
                pos += 1;
            }
            in_bad = false;
        } else {
            in_bad = true;
        }
    }

    // Trim trailing hyphen after truncation
    while (pos > 0 and result[pos - 1] == '-') {
        pos -= 1;
    }

    return result[0..pos];
}

fn isSlugChar(c: u8) bool {
    return (c >= 'a' and c <= 'z') or (c >= '0' and c <= '9');
}

test "baseName" {
    try std.testing.expectEqualStrings("pi", baseName("/usr/local/bin/pi"));
    try std.testing.expectEqualStrings("pytest", baseName("pytest"));
    try std.testing.expectEqualStrings("bash", baseName("/bin/bash"));
}

test "slugify" {
    const allocator = testing.allocator;

    var result = try slugify(allocator, "Hello World");
    defer allocator.free(result);
    try std.testing.expectEqualStrings("hello-world", result);

    result = try slugify(allocator, "My--Test!!");
    defer allocator.free(result);
    try std.testing.expectEqualStrings("my-test", result);

    result = try slugify(allocator, "a");
    defer allocator.free(result);
    try std.testing.expectEqualStrings("a", result);

    // Test empty result
    result = try slugify(allocator, "!!!");
    defer allocator.free(result);
    try std.testing.expectEqualStrings("", result);

    // Test long string truncation
    result = try slugify(allocator, "abcdefghijklmnopqrstuvwxyz01234567890123456789");
    defer allocator.free(result);
    try std.testing.expectEqual(@as(usize, 40), result.len);
}

const testing = std.testing;
