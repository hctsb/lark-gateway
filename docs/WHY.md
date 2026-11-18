# Why LarkGateway exists

## The problem

You have a few small programs on one machine, often an office server, and each needs to react when something happens in Lark: someone sends a bot a file, or someone ticks a box in a Base. You want the reaction within seconds, without opening the machine to the internet, and without losing an event when a program crashes, the machine reboots or the internet drops for ten minutes.

An example from our own use: an accountant sends the finance bot a bank statement file, and a program on the server imports it and replies in the chat within seconds. Before the gateway, one of our programs ran its own Lark listener, and another re-read a whole Base table every 2 minutes, around the clock (about 720 reads a day), just to notice changes.

## What it does

One process holds Lark's persistent connection (a WebSocket) for each of your Lark apps. For each event a rule matches, it runs a command you name, with the event as JSON on its input. It writes every event to disk before telling Lark it arrived, retries failures, sets aside and alerts on events that keep failing, and after every reconnect runs a catch-up command for whatever happened while it was offline. It makes no business decisions: your commands do.

## "Can't I just...?"

**...use Lark's webhook (HTTP callback) mode instead?** You can, if your machine has a public HTTPS address. An office server usually doesn't, and we didn't want to open one. The persistent connection needs only outgoing internet. Either way, something still has to keep the event safe and run your program.

**...put Lark's SDK in each program?** That works for one program. With two programs listening for the same Lark app, Lark hands each event to just one of the app's connections, at random, so each program silently misses some events. You need exactly one listener per app, which is what this is. (Lark's Python SDK also added about 170 MB and 20 seconds of start-up to one of our programs.)

**...use `lark-cli event subscribe` / `event consume`?** Good for watching events or feeding an AI agent. It prints events; it does not run your program per event, keep a copy on disk, retry, catch up after a disconnect or alert you. Wrapping it to do those things is what this project is.

**...use AnyCross, n8n, Make or Zapier?** They run in the cloud, or wherever you host them. To run a program on your own machine they need a way in, usually a public address, and you are back to the first question. If your work can be done entirely in the cloud, use them instead; this is for work that has to happen on a machine you own, for example because your accounting database is on it.

**...just poll Lark every minute?** Slower, uses up API quota, and gets heavier as tables grow. You still need to keep state to know what changed.

**...use a chat-bot bridge (an AI agent framework's Lark channel, or one of the "Feishu agent bridge" projects)?** Those connect Lark chats to an AI agent. Most handle chat messages only, and some drop every other event type, including Base record changes. They are not built to run a fixed command reliably.

## When not to use it

- You already run a public webhook endpoint with a queue behind it.
- Everything you do happens in the cloud: use AnyCross or similar.
- You need more than one machine to receive the same Lark app's events. Lark splits events between connections; this design assumes one machine per app.

## What is ours and what is not

The Lark connection is Lark's official Go SDK, and NSSM runs it as a Windows service. This project adds about 1,700 lines of Go: save first, one event at a time per route, retries, catch-up on reconnect, duplicate suppression, per-route settings, a health page and alerts.
