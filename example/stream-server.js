// Streaming interop peer for the Go bare-rpc test.
// Usage: bare stream-server.js <socketPath>
//
// Commands:
//   1  response stream  — server streams 3 chunks ("a","b","c") then ends.
//   2  request stream   — server reads the request stream, replies with the
//                         uppercased concatenation of everything it received.
//   3  bidirectional    — server echoes each request-stream chunk back through
//                         the response stream, reversed, then ends.

const RPC = require("bare-rpc");
const net = require("bare-net");
const process = require("bare-process");

const socketPath = process.argv[2];

const server = net.createServer((socket) => {
  socket.on("error", () => {});

  // eslint-disable-next-line no-new
  new RPC(socket, (req) => {
    switch (req.command) {
      case 1: {
        const reply = req.createResponseStream();
        reply.write(Buffer.from("a"));
        reply.write(Buffer.from("b"));
        reply.write(Buffer.from("c"));
        reply.end();
        break;
      }

      case 2: {
        const stream = req.createRequestStream();
        const chunks = [];
        stream
          .on("data", (d) => chunks.push(d))
          .on("end", () => {
            const all = Buffer.concat(chunks).toString().toUpperCase();
            req.reply(Buffer.from(all));
          });
        break;
      }

      case 3: {
        const inn = req.createRequestStream();
        const out = req.createResponseStream();
        inn
          .on("data", (d) => {
            out.write(Buffer.from(d.toString().split("").reverse().join("")));
          })
          .on("end", () => out.end());
        break;
      }
    }
  });
});

server.listen(socketPath);
