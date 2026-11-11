# Webhook receiver examples

Both verify `X-Sentinel-Signature: t=<unix>,v1=<hex hmac-sha256>` exactly as
`apps/processor-go/webhooks/dispatcher.go`'s `Sign()` computes it: HMAC-SHA256 over
`"<t>." + <raw body bytes>`, keyed by the webhook's raw secret, hex-encoded. See
`docs/agents/SENTINEL_AGENT_GUIDE.md` §10 for the worked example these implementations were
checked against (`t=1755000000` → `v1=0eaae859046e1eaa0b1b11ea58df505c693eb9435e009693ad5da8b608d5e6af`).

Both reject requests whose `t` is more than 5 minutes from the receiver's clock, to bound replay.

## Node (Express)

```js
const crypto = require('crypto');
const express = require('express');
const app = express();

app.use(express.raw({ type: 'application/json' })); // need the RAW bytes, not re-serialized JSON

app.post('/sentinel-webhook', (req, res) => {
  const header = req.header('X-Sentinel-Signature') || '';
  const match = header.match(/^t=(\d+),v1=([0-9a-f]{64})$/);
  if (!match) return res.status(400).end();

  const [, tsStr, sig] = match;
  const ts = Number(tsStr);
  if (Math.abs(Date.now() / 1000 - ts) > 300) return res.status(401).end(); // replay window

  const mac = crypto.createHmac('sha256', process.env.SENTINEL_WEBHOOK_SECRET);
  mac.update(`${tsStr}.`);
  mac.update(req.body); // raw Buffer
  const expected = mac.digest('hex');

  const ok = expected.length === sig.length &&
    crypto.timingSafeEqual(Buffer.from(expected), Buffer.from(sig));
  if (!ok) return res.status(401).end();

  const payload = JSON.parse(req.body.toString('utf8'));
  console.log(`delivery ${req.header('X-Sentinel-Delivery-Id')}: ${payload.events.length} events, cursor ${payload.cursor}`);
  res.status(200).end();
});

app.listen(3000);
```

## Python (Flask)

```python
import hmac, hashlib, time, os, json
from flask import Flask, request, abort

app = Flask(__name__)
SECRET = os.environ["SENTINEL_WEBHOOK_SECRET"].encode()

@app.post("/sentinel-webhook")
def receive():
    header = request.headers.get("X-Sentinel-Signature", "")
    try:
        parts = dict(p.split("=", 1) for p in header.split(","))
        ts, sig = parts["t"], parts["v1"]
    except (ValueError, KeyError):
        abort(400)

    if abs(time.time() - int(ts)) > 300:
        abort(401)  # outside the replay window

    mac = hmac.new(SECRET, f"{ts}.".encode() + request.get_data(), hashlib.sha256)
    if not hmac.compare_digest(mac.hexdigest(), sig):
        abort(401)

    payload = json.loads(request.get_data())
    print(f"delivery {request.headers.get('X-Sentinel-Delivery-Id')}: "
          f"{len(payload['events'])} events, cursor {payload['cursor']}")
    return "", 200
```
