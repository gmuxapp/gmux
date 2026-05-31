// gmuxd daemon - machine daemon
const std = @import("std");

pub const std_options = std.Options{
    .log_level = .info,
};

pub fn main(init: std.process.Init) !void {
    const allocator = init.gpa;
    const io = init.io;
    var it = try std.process.Args.Iterator.initAllocator(init.minimal.args, allocator);
    defer it.deinit();

    // Skip program name
    _ = it.next();

    var args_list = std.ArrayList([]const u8).empty;
    while (it.next()) |arg| {
        try args_list.append(allocator, try allocator.dupe(u8, arg));
    }
    defer {
        for (args_list.items) |arg| allocator.free(arg);
        args_list.deinit(allocator);
    }
    const args = args_list.items;

    if (args.len < 1) {
        printUsage(io, std.Io.File.stdout());
        std.process.exit(0);
        return;
    }

    const cmd = args[0];
    const rest = args[1..];

    const exit_code: u8 = blk: {
        if (std.mem.eql(u8, cmd, "start") or std.mem.eql(u8, cmd, "restart")) {
            break :blk @intCast(try cmdStart(allocator, rest));
        } else if (std.mem.eql(u8, cmd, "run")) {
            break :blk @intCast(try cmdRun(allocator, rest));
        } else if (std.mem.eql(u8, cmd, "stop")) {
            break :blk @intCast(try cmdStop(allocator, rest));
        } else if (std.mem.eql(u8, cmd, "status")) {
            break :blk @intCast(try cmdStatus(allocator, rest));
        } else if (std.mem.eql(u8, cmd, "auth")) {
            break :blk @intCast(try cmdAuth(allocator, rest));
        } else if (std.mem.eql(u8, cmd, "remote")) {
            break :blk @intCast(try cmdRemote(allocator, rest));
        } else if (std.mem.eql(u8, cmd, "version")) {
            break :blk @intCast(try cmdVersion(io, std.Io.File.stdout()));
        } else if (std.mem.eql(u8, cmd, "log-path")) {
            break :blk @intCast(try cmdLogPath(allocator, rest));
        } else if (std.mem.eql(u8, cmd, "help") or std.mem.eql(u8, cmd, "-h") or std.mem.eql(u8, cmd, "--help")) {
            printUsage(io, std.Io.File.stdout());
            break :blk 0;
        } else {
            const stderr = std.Io.File.stderr();
            stderr.writeStreamingAll(io, "gmuxd: unknown command") catch {};
            stderr.writeStreamingAll(io, ": \"") catch {};
            stderr.writeStreamingAll(io, cmd) catch {};
            stderr.writeStreamingAll(io, "\"\n") catch {};
            printUsage(io, stderr);
            break :blk 2;
        }
    };

    std.process.exit(exit_code);
}

fn printUsage(io: std.Io, file: std.Io.File) void {
    const msg =
        \\gmuxd - machine daemon
        \\
        \\Usage: gmuxd <command>
        \\
        \\Commands:
        \\  start              Start the daemon in the background
        \\  run                Run the daemon in the foreground
        \\  stop               Stop the running daemon
        \\  restart            Restart the daemon
        \\  status             Show daemon health
        \\  auth               Show the auth URL and token
        \\  remote             Set up remote access via Tailscale
        \\  log-path           Print the daemon log file path
        \\  version            Show gmuxd version
        \\  help               Show this help
        \\
    ;
    file.writeStreamingAll(io, msg) catch {};
}

fn cmdStart(allocator: Allocator, args: []const []const u8) !u32 {
    _ = allocator;
    _ = args;
    std.debug.print("Starting gmuxd...\n", .{});
    return 0;
}

fn cmdRun(allocator: Allocator, args: []const []const u8) !u32 {
    _ = allocator;
    _ = args;
    std.debug.print("Running gmuxd in foreground...\n", .{});
    return 0;
}

fn cmdStop(allocator: Allocator, args: []const []const u8) !u32 {
    _ = allocator;
    _ = args;
    std.debug.print("Stopping gmuxd...\n", .{});
    return 0;
}

fn cmdStatus(allocator: Allocator, args: []const []const u8) !u32 {
    _ = allocator;
    _ = args;
    return 0;
}

fn cmdAuth(allocator: Allocator, args: []const []const u8) !u32 {
    _ = allocator;
    _ = args;
    return 0;
}

fn cmdRemote(allocator: Allocator, args: []const []const u8) !u32 {
    _ = allocator;
    _ = args;
    return 0;
}

fn cmdVersion(io: std.Io, file: std.Io.File) !u32 {
    file.writeStreamingAll(io, "dev\n") catch {};
    return 0;
}

fn cmdLogPath(allocator: Allocator, args: []const []const u8) !u32 {
    _ = allocator;
    _ = args;
    return 0;
}

const Allocator = std.mem.Allocator;
