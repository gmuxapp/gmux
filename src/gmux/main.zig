// gmux CLI - session launcher
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
        printUsage(io, std.Io.File.stderr());
        std.process.exit(2);
        return;
    }

    const mode = try parseMode(args[0]);

    switch (mode) {
        .help => {
            printUsage(io, std.Io.File.stdout());
        },
        .ui => {
            try openUI(allocator);
        },
        .run => {
            var flags = Flags.init();
            try flags.parse(args[1..]);
            const command = args[flags.argCount + 1..];
            try runSession(allocator, command, flags);
        },
        .list => {
            var flags = Flags.init();
            try flags.parse(args[1..]);
            const code = try cmdList(allocator, flags);
            std.process.exit(@intCast(code));
        },
        .kill => {
            var flags = Flags.init();
            try flags.parse(args[1..]);
            if (args.len < flags.argCount + 2) {
                std.Io.File.stderr().writeStreamingAll(io, "gmux: session id required\n") catch {};
                std.process.exit(2);
                return;
            }
            const sessionId = args[flags.argCount + 1];
            const code = try cmdKill(allocator, sessionId, flags);
            std.process.exit(@intCast(code));
        },
        .tail => {
            var flags = Flags.init();
            try flags.parse(args[1..]);
            if (args.len < flags.argCount + 2) {
                std.Io.File.stderr().writeStreamingAll(io, "gmux: session id required\n") catch {};
                std.process.exit(2);
                return;
            }
            const sessionId = args[flags.argCount + 1];
            const code = try cmdTail(allocator, sessionId, flags);
            std.process.exit(@intCast(code));
        },
        .attach => {
            var flags = Flags.init();
            try flags.parse(args[1..]);
            if (args.len < flags.argCount + 2) {
                std.Io.File.stderr().writeStreamingAll(io, "gmux: session id required\n") catch {};
                std.process.exit(2);
                return;
            }
            const sessionId = args[flags.argCount + 1];
            const code = try cmdAttach(allocator, sessionId, flags);
            std.process.exit(@intCast(code));
        },
        .send => {
            var flags = Flags.init();
            try flags.parse(args[1..]);
            if (args.len < flags.argCount + 2) {
                std.Io.File.stderr().writeStreamingAll(io, "gmux: session id required\n") catch {};
                std.process.exit(2);
                return;
            }
            const sessionId = args[flags.argCount + 1];
            const text: ?[]const u8 = if (args.len > flags.argCount + 2) args[flags.argCount + 2] else null;
            const code = try cmdSend(allocator, sessionId, text, flags);
            std.process.exit(@intCast(code));
        },
        .wait => {
            var flags = Flags.init();
            try flags.parse(args[1..]);
            if (args.len < flags.argCount + 2) {
                std.Io.File.stderr().writeStreamingAll(io, "gmux: session id required\n") catch {};
                std.process.exit(2);
                return;
            }
            const sessionId = args[flags.argCount + 1];
            const code = try cmdWait(allocator, sessionId, flags);
            std.process.exit(@intCast(code));
        },
        .dumpEnv => {
            const code = try cmdDumpEnv(allocator);
            std.process.exit(@intCast(code));
        },
    }
}

const Mode = enum {
    help,
    ui,
    run,
    list,
    kill,
    tail,
    attach,
    send,
    wait,
    dumpEnv,
};

fn parseMode(arg: []const u8) !Mode {
    if (std.mem.eql(u8, arg, "--help") or std.mem.eql(u8, arg, "-h")) return .help;
    if (std.mem.eql(u8, arg, "list") or std.mem.eql(u8, arg, "ls")) return .list;
    if (std.mem.eql(u8, arg, "kill")) return .kill;
    if (std.mem.eql(u8, arg, "tail")) return .tail;
    if (std.mem.eql(u8, arg, "attach")) return .attach;
    if (std.mem.eql(u8, arg, "send")) return .send;
    if (std.mem.eql(u8, arg, "wait")) return .wait;
    if (std.mem.eql(u8, arg, "dump-env")) return .dumpEnv;
    return .run;
}

const Flags = struct {
    noAttach: bool = false,
    resumeID: ?[]const u8 = null,
    initialCols: u16 = 0,
    initialRows: u16 = 0,
    host: ?[]const u8 = null,
    all: bool = false,
    tail: u16 = 0,
    noSubmit: bool = false,
    waitTimeout: u32 = 0,
    argCount: usize = 0,

    pub fn init() Flags {
        return .{};
    }

    pub fn parse(self: *Flags, args: []const []const u8) !void {
        var i: usize = 0;
        while (i < args.len) : (i += 1) {
            const arg = args[i];
            if (std.mem.startsWith(u8, arg, "--")) {
                if (std.mem.eql(u8, arg, "--no-attach")) {
                    self.noAttach = true;
                } else if (std.mem.startsWith(u8, arg, "--resume-id=")) {
                    self.resumeID = arg["--resume-id=".len..];
                } else if (std.mem.startsWith(u8, arg, "--initial-cols=")) {
                    self.initialCols = try std.fmt.parseInt(u16, arg["--initial-cols=".len..], 10);
                } else if (std.mem.startsWith(u8, arg, "--initial-rows=")) {
                    self.initialRows = try std.fmt.parseInt(u16, arg["--initial-rows=".len..], 10);
                } else if (std.mem.startsWith(u8, arg, "--host=")) {
                    self.host = arg["--host=".len..];
                } else if (std.mem.eql(u8, arg, "--all")) {
                    self.all = true;
                } else if (std.mem.startsWith(u8, arg, "--tail=")) {
                    self.tail = try std.fmt.parseInt(u16, arg["--tail=".len..], 10);
                } else if (std.mem.eql(u8, arg, "--no-submit")) {
                    self.noSubmit = true;
                } else if (std.mem.startsWith(u8, arg, "--timeout=")) {
                    self.waitTimeout = try std.fmt.parseInt(u32, arg["--timeout=".len..], 10);
                }
            } else if (std.mem.startsWith(u8, arg, "-")) {
                var j: usize = 1;
                while (j < arg.len) : (j += 1) {
                    switch (arg[j]) {
                        'a' => self.noAttach = true,
                        else => {},
                    }
                }
            } else {
                self.argCount = i;
                break;
            }
        }
    }
};

fn printUsage(io: std.Io, file: std.Io.File) void {
    const msg =
        \\gmux - session launcher
        \\
        \\Usage: gmux [command] [options] <args...>
        \\
        \\Commands:
        \\  <command>          Launch a command as a managed session
        \\  ls                 List sessions
        \\  kill <id>          Kill a session
        \\  tail <id>          Show tail of session output
        \\  attach <id>        Attach to a session
        \\  send <id> [text]   Send input to a session
        \\  wait <id>          Wait for a session to exit
        \\  dump-env           Print session environment
        \\  --help             Show this help
        \\
    ;
    file.writeStreamingAll(io, msg) catch {};
}

fn openUI(allocator: Allocator) !void {
    _ = allocator;
    std.debug.print("Opening gmux UI...\n", .{});
}

fn runSession(allocator: Allocator, command: []const []const u8, flags: Flags) !void {
    _ = allocator;
    _ = command;
    _ = flags;
    std.debug.print("Running session...\n", .{});
}

fn cmdList(allocator: Allocator, flags: Flags) !u32 {
    _ = allocator;
    _ = flags;
    return 0;
}

fn cmdKill(allocator: Allocator, sessionId: []const u8, flags: Flags) !u32 {
    _ = allocator;
    _ = sessionId;
    _ = flags;
    return 0;
}

fn cmdTail(allocator: Allocator, sessionId: []const u8, flags: Flags) !u32 {
    _ = allocator;
    _ = sessionId;
    _ = flags;
    return 0;
}

fn cmdAttach(allocator: Allocator, sessionId: []const u8, flags: Flags) !u32 {
    _ = allocator;
    _ = sessionId;
    _ = flags;
    return 0;
}

fn cmdSend(allocator: Allocator, sessionId: []const u8, text: ?[]const u8, flags: Flags) !u32 {
    _ = allocator;
    _ = sessionId;
    _ = text;
    _ = flags;
    return 0;
}

fn cmdWait(allocator: Allocator, sessionId: []const u8, flags: Flags) !u32 {
    _ = allocator;
    _ = sessionId;
    _ = flags;
    return 0;
}

fn cmdDumpEnv(allocator: Allocator) !u32 {
    _ = allocator;
    return 0;
}

const Allocator = std.mem.Allocator;
