const http = require("http");

const port = process.env.PORT || 3000;
let hits = 0;

http
  .createServer((req, res) => {
    hits++;
    res.setHeader("content-type", "application/json");
    res.end(JSON.stringify({ app: "node-http", hits, database: Boolean(process.env.DATABASE_URL), heap: process.env.NODE_OPTIONS || "" }));
  })
  .listen(port, "0.0.0.0", () => console.log(`listening on ${port}`));
