// app.ts — the HTTP server and router. No business logic lives here: the
// request body is read whole, then the path dispatches to one surface
// (git smart HTTP, REST v3, GraphQL v4, MCP), each in its own module, over
// the one shared in-memory store.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { handleGit } from './git-http.ts';
import { handleRest } from './rest.ts';
import { handleGraphQL } from './graphql.ts';
import { handleMcp } from './mcp.ts';
import { Store } from './store.ts';

const JSON_TYPE = 'application/json; charset=utf-8';

function sendJson404(res: ServerResponse): void {
  const text = `${JSON.stringify({ message: 'Not Found' })}\n`;
  res.writeHead(404, { 'content-type': JSON_TYPE, 'content-length': Buffer.byteLength(text) });
  res.end(text);
}

/** Build the server. It binds nothing until the caller listens. */
export function createApp(): Server {
  const store = new Store();
  return createServer((req: IncomingMessage, res: ServerResponse) => {
    const chunks: Buffer[] = [];
    req.on('data', (chunk: Buffer) => chunks.push(chunk));
    req.on('error', () => {
      if (!res.writableEnded) {
        res.writeHead(400, { 'content-type': JSON_TYPE });
        res.end(`${JSON.stringify({ message: 'Bad Request' })}\n`);
      }
    });
    req.on('end', () => {
      const body = Buffer.concat(chunks);
      let pathname = '/';
      let search = new URLSearchParams();
      try {
        const url = new URL(req.url ?? '/', 'http://entry');
        pathname = url.pathname;
        search = url.searchParams;
      } catch {
        pathname = '/';
      }
      const method = req.method ?? 'GET';
      try {
        // The env's boot check calls this twice: 200 {"ok":true}.
        if (method === 'GET' && pathname === '/healthz') {
          const text = `${JSON.stringify({ ok: true })}\n`;
          res.writeHead(200, { 'content-type': JSON_TYPE, 'content-length': Buffer.byteLength(text) });
          res.end(text);
          return;
        }
        if (handleGit(store, req, res, pathname, search, body)) return;
        if (handleGraphQL(store, req, res, pathname, body)) return;
        if (handleMcp(store, req, res, pathname, body)) return;
        if (handleRest(store, req, res, pathname, search, body)) return;
        sendJson404(res);
      } catch {
        if (!res.writableEnded) sendJson404(res);
      }
    });
  });
}
