// Bot-driver simulator. Each bot is a real driver persona driving a plausible
// looping route around Bengaluru and pinging its location through the PUBLIC
// API at ~1/sec — exactly what a real driver app does. Bots also listen for
// ride offers on their driver SSE channel and auto-accept after a short delay,
// then drive to the pickup and mark arriving/arrived. Bots deliberately do NOT
// start or complete trips (that needs the rider's OTP, surfaced only on the
// rider panel) — they are realistic, matchable supply plus map ambiance.
//
// No backend changes: everything here is ordinary API traffic.

import { Api } from "../api/client";
import type { DriverPersona } from "../config/personas";
import { CITY_BOUNDS } from "../config/personas";
import type { OfferData, SSEEnvelope, StatusChangedData } from "../api/types";
import { haversine, lerp, type LatLng } from "../lib/geo";
import { openStream, type SSEHandle } from "../sse/stream";

type BotMode = "roam" | "to_pickup" | "waiting";

interface Bot {
  persona: DriverPersona;
  api: Api;
  pos: LatLng;
  target: LatLng;
  mode: BotMode;
  rideId: string | null;
  pickup: LatLng | null;
  offerSse: SSEHandle | null;
  rideSse: SSEHandle | null;
  arrivedSent: boolean;
  arrivingSent: boolean;
}

// Bot cruising speed in metres per tick (~1s). ~11 m/s ≈ 40 km/h.
const SPEED_M = 220;
const PICKUP_RADIUS_M = 90;

function randInBounds(): LatLng {
  const { minLat, maxLat, minLng, maxLng } = CITY_BOUNDS;
  return [minLat + Math.random() * (maxLat - minLat), minLng + Math.random() * (maxLng - minLng)];
}

export class Simulator {
  private bots: Bot[] = [];
  private tick: ReturnType<typeof setInterval> | null = null;
  private subs = new Set<() => void>();
  private running = false;

  isRunning() {
    return this.running;
  }

  subscribe(cb: () => void): () => void {
    this.subs.add(cb);
    return () => this.subs.delete(cb);
  }

  private notify() {
    this.subs.forEach((cb) => cb());
  }

  getBots(): { id: string; pos: LatLng }[] {
    return this.bots.map((b) => ({ id: b.persona.id, pos: b.pos }));
  }

  start(personas: DriverPersona[]) {
    this.stop();
    this.running = true;
    this.bots = personas.map((p) => {
      const start = randInBounds();
      const bot: Bot = {
        persona: p,
        api: new Api(p.token),
        pos: start,
        target: randInBounds(),
        mode: "roam",
        rideId: null,
        pickup: null,
        offerSse: null,
        rideSse: null,
        arrivedSent: false,
        arrivingSent: false,
      };
      // Go online, then listen for offers.
      bot.api.setAvailability(p.id, true).catch(() => {});
      bot.offerSse = openStream(`/v1/events/driver/${p.id}`, p.token, {
        onEvent: (env) => this.onOfferEvent(bot, env),
      });
      return bot;
    });

    this.tick = setInterval(() => this.step(), 1000);
    this.notify();
  }

  stop() {
    this.running = false;
    if (this.tick) clearInterval(this.tick);
    this.tick = null;
    for (const b of this.bots) {
      b.offerSse?.close();
      b.rideSse?.close();
      b.api.setAvailability(b.persona.id, false).catch(() => {});
    }
    this.bots = [];
    this.notify();
  }

  private onOfferEvent(bot: Bot, env: SSEEnvelope) {
    if (env.type !== "ride.offer") return;
    const data = env.data as OfferData;
    if (bot.mode !== "roam") return; // already busy
    // Auto-accept after 2–4s to mimic a human reacting to the offer.
    const delay = 2000 + Math.random() * 2000;
    const rideId = data.ride_id;
    setTimeout(async () => {
      if (bot.mode !== "roam") return;
      try {
        await bot.api.accept(bot.persona.id, rideId);
        bot.mode = "to_pickup";
        bot.rideId = rideId;
        bot.pickup = [data.pickup_lat, data.pickup_lng];
        bot.target = bot.pickup;
        bot.arrivingSent = false;
        bot.arrivedSent = false;
        // Watch the ride so we can go back to roaming when it ends/cancels.
        bot.rideSse = openStream(`/v1/events?ride_id=${rideId}`, bot.persona.token, {
          onEvent: (e) => this.onRideEvent(bot, e),
        });
      } catch {
        // Offer likely expired or was claimed elsewhere — keep roaming.
      }
    }, delay);
  }

  private onRideEvent(bot: Bot, env: SSEEnvelope) {
    if (env.type !== "ride.status_changed") return;
    const st = (env.data as StatusChangedData).status;
    if (st === "COMPLETED" || st === "CANCELLED_BY_RIDER" || st === "CANCELLED_BY_DRIVER" || st === "EXPIRED") {
      this.resetBot(bot);
    }
  }

  private resetBot(bot: Bot) {
    bot.rideSse?.close();
    bot.rideSse = null;
    bot.mode = "roam";
    bot.rideId = null;
    bot.pickup = null;
    bot.target = randInBounds();
    // Re-assert availability (trip end frees us, but this is harmless/idempotent).
    bot.api.setAvailability(bot.persona.id, true).catch(() => {});
  }

  private step() {
    for (const bot of this.bots) {
      this.drive(bot);
      // Ping location through the public API (drives matching + rider tracking).
      bot.api.ping(bot.persona.id, bot.pos[0], bot.pos[1]).catch(() => {});

      if (bot.mode === "to_pickup" && bot.pickup) {
        const dist = haversine(bot.pos, bot.pickup);
        if (dist < PICKUP_RADIUS_M * 2 && !bot.arrivingSent && bot.rideId) {
          bot.arrivingSent = true;
          bot.api.arriving(bot.rideId).catch(() => {});
        }
        if (dist < PICKUP_RADIUS_M && !bot.arrivedSent && bot.rideId) {
          bot.arrivedSent = true;
          // small delay so ARRIVING is visible before ARRIVED
          setTimeout(() => bot.rideId && bot.api.arrived(bot.rideId).catch(() => {}), 1200);
          bot.mode = "waiting"; // parked at pickup; rider-side OTP starts the trip
        }
      }
    }
    this.notify();
  }

  private drive(bot: Bot) {
    if (bot.mode === "waiting") return; // parked
    const d = haversine(bot.pos, bot.target);
    if (d < SPEED_M) {
      bot.pos = bot.target;
      if (bot.mode === "roam") bot.target = randInBounds();
      return;
    }
    bot.pos = lerp(bot.pos, bot.target, SPEED_M / d);
  }
}

// One shared simulator instance for the app.
export const simulator = new Simulator();
