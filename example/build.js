import HRPCBuilder from "hrpc";
import Hyperschema from "hyperschema";

export const schema = Hyperschema.from("./schema", { import: false });

{
  const ns = schema.namespace("example");

  ns.register({
    name: "hello-request",
    fields: [
      {
        name: "text",
        type: "string",
        required: true,
      },
      {
        name: "from",
        type: "string",
        required: true,
      },
      {
        name: "age",
        type: "int",
        required: true,
      },
      {
        name: "happy",
        type: "bool",
        required: true,
      },
      {
        name: "dogs",
        type: "string",
        array: true,
        required: true,
      },
    ],
  });

  ns.register({
    name: "hello-response",
    fields: [
      {
        name: "reply",
        type: "string",
      },
    ],
  });
}

Hyperschema.toDisk(schema, { esm: false });
Hyperschema.toDisk(schema, { esm: false, lang: "go" });

const builder = HRPCBuilder.from("./schema", "./hrpc");
{
  const ns = builder.namespace("example");

  ns.register({
    name: "hello",
    request: { name: "@example/hello-request" },
    response: { name: "@example/hello-response" },
  });
}
HRPCBuilder.toDisk(builder, { esm: false });
