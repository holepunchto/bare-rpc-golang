const RPC = require("bare-rpc");
const net = require("bare-net");
const process = require("bare-process");
const socketPath = process.argv[2];
const c = require("compact-encoding");

const Item = {
  preencode(state, m) {
    c.string.preencode(state, m.title);
    c.string.preencode(state, m.desc);
  },
  encode(state, m) {
    c.string.encode(state, m.title);
    c.string.encode(state, m.desc);
  },
  decode(state) {
    const title = c.string.decode(state);
    const desc = c.string.decode(state);

    return {
      title,
      desc,
    };
  },
};

const Items = c.array(Item);

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

    switch (req.command) {
      case 0: {
        req.reply(
          c.encode(Items, [
            { title: "Raspberry Pi’s", desc: "I have ’em all over my house" },
            { title: "Nutella", desc: "It's good on toast" },
            { title: "Bitter melon", desc: "It cools you down" },
            {
              title: "Nice socks",
              desc: "And by that I mean socks without holes",
            },
            { title: "Eight hours of sleep", desc: "I had this once" },
            { title: "Cats", desc: "Usually" },
            { title: "Plantasia, the album", desc: "My plants love it too" },
            {
              title: "Pour over coffee",
              desc: "It takes forever to make though",
            },
            { title: "VR", desc: "Virtual reality...what is there to say?" },
            { title: "Noguchi Lamps", desc: "Such pleasing organic forms" },
            { title: "Linux", desc: "Pretty much the best OS" },
            { title: "Business school", desc: "Just kidding" },
            { title: "Pottery", desc: "Wet clay is a great feeling" },
            { title: "Shampoo", desc: "Nothing like clean hair" },
            { title: "Table tennis", desc: "It’s surprisingly exhausting" },
            {
              title: "Milk crates",
              desc: "Great for packing in your extra stuff",
            },
            {
              title: "Afternoon tea",
              desc: "Especially the tea sandwich part",
            },
            { title: "Stickers", desc: "The thicker the vinyl the better" },
            { title: "20° Weather", desc: "Celsius, not Fahrenheit" },
            { title: "Warm light", desc: "Like around 2700 Kelvin" },
            {
              title: "The vernal equinox",
              desc: "The autumnal equinox is pretty good too",
            },
            { title: "Gaffer’s tape", desc: "Basically sticky fabric" },
            { title: "Terrycloth", desc: "In other words, towel fabric" },
          ]),
        );

        break;
      }
      case 42: {
        req.reply(Buffer.from("hello from Bare"));
        break;
      }
    }
  });
});

server.listen(socketPath);
