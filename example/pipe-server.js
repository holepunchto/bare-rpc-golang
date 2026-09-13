// Bare side of example/pipe: serves bare-rpc on the socketpair end inherited as fd 3.
const RPC = require("bare-rpc");
const Pipe = require("bare-pipe");

const pipe = new Pipe(3);

new RPC(pipe, (req) => {
  if (req.command === 1) req.reply(Buffer.from("pong: " + req.data));
});
