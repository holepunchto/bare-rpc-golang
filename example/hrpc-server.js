const net = require("bare-net");
const process = require("bare-process");
const socketPath = process.argv[2];
const HRPC = require("./hrpc/index.js");

const server = net.createServer(async (socket) => {
  socket.on("error", (err) => {
    console.error("Bare: socket error:", err);
  });

  const rpc = new HRPC(socket);

  console.log("ready?");
  rpc.onHello((res) => {
    console.log(res);

    return { reply: "world!" };
  });

  await new Promise((res) => setTimeout(res, 1000));

  const res = await rpc.hello({
    text: "Hello!",
    from: "Me",
    age: 20,
    happy: true,
    dogs: ["one", "two"],
  });
  console.log("REPLY to bare", res);
});

server.listen(socketPath);
