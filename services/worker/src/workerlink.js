// Worker side of the worker<->Control channel (mTLS). Phase-0 scope: dial the Control presenting a
// client cert (mTLS), REGISTER with id + one-time token, then HEARTBEAT on an interval.
// No SMTP, no job stream. Uses Node's built-in https/tls only — no third-party deps.

import https from "node:https";
import fs from "node:fs";

export class WorkerLink {
  /**
   * @param {object} o
   * @param {string} o.host  Control host
   * @param {number} o.port  Control mTLS port
   * @param {string} o.workerId
   * @param {string} o.token one-time registration token (authority)
   * @param {string} o.certPath worker client cert
   * @param {string} o.keyPath  worker client key
   * @param {string} o.caPath   CA that signed the Control server cert
   */
  constructor(o) {
    this.host = o.host;
    this.port = o.port;
    this.workerId = o.workerId;
    this.token = o.token;
    // mTLS material: our client cert/key + the CA we verify the Control against.
    this.tls = {
      cert: fs.readFileSync(o.certPath),
      key: fs.readFileSync(o.keyPath),
      ca: fs.readFileSync(o.caPath),
      // Verify the Control's server cert against our CA; SAN is "control"/"localhost".
      rejectUnauthorized: true,
      servername: o.serverName || "control",
    };
  }

  #post(path, bodyObj) {
    const body = Buffer.from(JSON.stringify(bodyObj), "utf8");
    const opts = {
      host: this.host, port: this.port, path, method: "POST",
      headers: { "content-type": "application/json", "content-length": body.length },
      cert: this.tls.cert, key: this.tls.key, ca: this.tls.ca,
      rejectUnauthorized: this.tls.rejectUnauthorized, servername: this.tls.servername,
    };
    return new Promise((resolve, reject) => {
      const req = https.request(opts, (res) => {
        let data = "";
        res.on("data", (c) => (data += c));
        res.on("end", () => resolve({ status: res.statusCode, body: data }));
      });
      req.on("error", reject);
      req.write(body);
      req.end();
    });
  }

  /** Register: token is the authorization authority; the client cert is transport. */
  async register() {
    const r = await this.#post("/v1/worker/register", { box_id: this.workerId, token: this.token });
    if (r.status !== 200) throw new Error(`register failed: status ${r.status}`);
    return true;
  }

  /** One heartbeat. */
  async heartbeat() {
    const r = await this.#post("/v1/worker/heartbeat", { box_id: this.workerId });
    if (r.status !== 200) throw new Error(`heartbeat failed: status ${r.status}`);
    return true;
  }
}
