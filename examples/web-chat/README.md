# Web chat example

One HTML file, no build: a small client of the Marpokat protocol. Use it to try the chat by hand, and as the
reference for the client rules of [`spec/events.md`](../../spec/events.md): the cursor and gap rule per channel,
reconnecting with jitter, resending unacknowledged messages with the same `client_msg_id`, ping every 25 seconds.

## Run

```bash
cd examples/web-chat
python3 -m http.server 3000      # port 3000: the Narrativ dev API allows that origin for its sign in
```

Open <http://localhost:3000> in two browsers (or one normal and one private window), one per user.

**With Narrativ dev:** open "Get a token from Narrativ", enter the Narrativ dev API
(`https://api-dev-narrativ.rizkysiregar.my.id/api/v1`) and an account, press Sign in, then Connect. The page fills in
the Marpokat URL, api key and token from `POST /chat/token`.

**Without Narrativ:** make the values yourself against any Marpokat:

```bash
marpokat tenant token --slug my-app --user alice      # user token, valid 1 hour
```

The api key is in `marpokat tenant show --slug my-app`, and both users must exist (`PUT /v1/server/users/{id}`) and
share a channel (`POST /v1/server/channels/dm`).

## Things to try

- Send from one browser: the other shows it at once, with the unread badge while another channel is open.
- Type in one: the other shows "typing…".
- Close one browser: the other's green dot goes grey.
- Stop a pod (or `docker compose restart` the API): the page reconnects with a short random delay and catches up.
  Messages you send while offline stay grey until they are acknowledged, and are sent once, whatever happens.
- Open the "Frames" panel to see every frame in both directions.
- The token lasts an hour: after that the server closes with 4004 and the page asks for a new one.
