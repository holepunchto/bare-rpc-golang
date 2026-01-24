const RPC = require("bare-rpc");
const net = require("bare-net");
const process = require("bare-process");
const socketPath = process.argv[2];
const c = require("compact-encoding");

const Message = {
  preencode(state, m) {
    c.uint.preencode(state, m.type);
    c.string.preencode(state, m.value);
  },
  encode(state, m) {
    c.uint.encode(state, m.type);
    c.string.encode(state, m.value);
  },
  decode(state) {
    const r0 = c.uint.decode(state);
    const r1 = c.string.decode(state);

    return {
      type: r0,
      value: r1,
    };
  },
};

const server = net.createServer(async (socket) => {
  socket.on("error", (err) => {
    console.error("Bare: socket error:", err);
  });

  const rpc = new RPC(socket, (req) => {
    console.error(
      "Bare: got request command=%d data=%s",
      req.command,
      req.data?.toString(),
    );
    req.reply(Buffer.from("hello from Bare"));
  });

  const req = rpc.request(24);
  req.send(
    c.encode(Message, { type: 0, value: "Hello from compact-encoding!!" }),
  );

  const replyBuffer = await req.reply();
  console.log(replyBuffer.toString());
});

server.listen(socketPath);
