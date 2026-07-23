import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Api } from "../api/client";
import type { OfferData, RideStatus, RideView, SSEEnvelope, StatusChangedData } from "../api/types";
import type { DriverPersona } from "../config/personas";
import { DRIVERS, placeName } from "../config/personas";
import { playChirp, startTitleFlash, stopTitleFlash } from "../lib/alerts";
import { buildRoute, cumulativeDistances, haversine, pointAtDistanceCum, type LatLng } from "../lib/geo";
import { rupees } from "../lib/money";
import { fetchRoad } from "../lib/routing";
import { MapView, type BotMarker } from "../map/MapView";
import { openStream } from "../sse/stream";
import { ConfirmDialog } from "../ui/dialog";
import { Spinner } from "../ui/spinner";
import { useToast } from "../ui/toast";

// Metres advanced along the route per ~1s tick. Demo-brisk on purpose (this is
// a watchable simulation console, not a real-time clock) — the car traces the
// real OSRM road geometry, just faster than wall-clock driving. The *displayed*
// ETA is computed from a realistic city speed (below), so it reads in true
// minutes even though the car arrives sooner.
const DRIVE_SPEED_M = 180;
// ~30 km/h city speed, used only to turn remaining route distance into a
// human ETA ("~4 min away"). OSRM's own duration/distance is preferred when
// available; this is the offline fallback.
const CITY_SPEED_MPS = 30000 / 3600;
const NEAR_PICKUP_M = 90;
const OFFER_TITLE = "🚗 New ride request — GoRide";

interface Props {
  persona: DriverPersona;
  onPersonaChange: (id: string) => void;
  lastRiderPickup: LatLng | null;
  bots: BotMarker[];
}

type Phase = "idle" | "to_pickup" | "at_pickup" | "to_drop";

// The active leg the drive loop is tracing.
interface Leg {
  phase: Phase;
  pts: LatLng[];
  cum: number[]; // cumulative distance per vertex (precomputed once per leg)
  cumTotal: number; // total leg length in metres
  driven: number; // metres advanced from the leg start
  speedMps: number; // realistic speed for ETA (OSRM-derived when available)
}

// Build a Leg with its cumulative-distance index precomputed (O(1)-ish
// per-frame lookups during the 60fps drive loop).
function makeLeg(phase: Phase, pts: LatLng[], driven: number, speedMps: number): Leg {
  const cum = cumulativeDistances(pts);
  return { phase, pts, cum, cumTotal: cum[cum.length - 1] ?? 0, driven, speedMps };
}

function phaseFor(status: RideStatus | null): Phase {
  switch (status) {
    case "DRIVER_ASSIGNED":
    case "DRIVER_ARRIVING":
      return "to_pickup";
    case "ARRIVED":
      return "at_pickup";
    case "IN_PROGRESS":
      return "to_drop";
    default:
      return "idle";
  }
}

export function DriverPanel({ persona, onPersonaChange, lastRiderPickup, bots }: Props) {
  const toast = useToast();
  const api = useMemo(() => new Api(persona.token), [persona.token]);

  const [online, setOnline] = useState(false);
  const [pos, setPos] = useState<LatLng>([12.9756, 77.6068]); // MG Road
  const [carBrg, setCarBrg] = useState(0); // heading of the car glyph
  const [collapsed, setCollapsed] = useState(false); // overlay card minimized
  const [ride, setRide] = useState<RideView | null>(null);
  const [status, setStatus] = useState<RideStatus | null>(null);
  const [offer, setOffer] = useState<OfferData | null>(null);
  const [countdown, setCountdown] = useState(1);
  const [otpInput, setOtpInput] = useState("");
  const [autopilot, setAutopilot] = useState(true);
  const [busy, setBusy] = useState<string | null>(null); // which action is in flight
  const [booting, setBooting] = useState(true); // panel-level rehydration shimmer
  const [eta, setEta] = useState<number | null>(null); // seconds to pickup
  const [legPath, setLegPath] = useState<LatLng[] | null>(null); // drawn route
  const [reconnecting, setReconnecting] = useState(false);
  const [confirm, setConfirm] = useState<null | "decline" | "end">(null);

  // Refs the 1s driving loop reads (avoids stale closures).
  const posRef = useRef(pos);
  const rideRef = useRef<RideView | null>(null);
  const statusRef = useRef<RideStatus | null>(null);
  const autopilotRef = useRef(autopilot);
  const sentRef = useRef<{ arriving: boolean; arrived: boolean }>({ arriving: false, arrived: false });
  const legRef = useRef<Leg | null>(null);
  const tripStartRef = useRef<number | null>(null);
  const sseDownRef = useRef<{ offer: boolean; ride: boolean }>({ offer: false, ride: false });

  useEffect(() => { posRef.current = pos; }, [pos]);
  useEffect(() => { rideRef.current = ride; }, [ride]);
  useEffect(() => { statusRef.current = status; }, [status]);
  useEffect(() => { autopilotRef.current = autopilot; }, [autopilot]);

  const phase = phaseFor(status);

  const setSseStatus = useCallback((which: "offer" | "ride", s: "open" | "reconnecting") => {
    sseDownRef.current[which] = s === "reconnecting";
    setReconnecting(sseDownRef.current.offer || sseDownRef.current.ride);
  }, []);

  // Reset on persona switch, then rehydrate an in-flight assignment: a page
  // refresh must not orphan a driver mid-ride (the backend still has them
  // on_trip; the client re-attaches and the drive loop resumes).
  useEffect(() => {
    setBooting(true);
    setOnline(false);
    setRide(null);
    setStatus(null);
    setOffer(null);
    setOtpInput("");
    stopTitleFlash();
    tripStartRef.current = null;

    let stale = false;
    (async () => {
      try {
        const st = await api.driverState(persona.id);
        if (stale) return;
        if (st.active_ride) {
          setRide(st.active_ride);
          setStatus(st.active_ride.status);
          setOnline(true); // on_trip server-side
          if (st.active_ride.status === "IN_PROGRESS") tripStartRef.current = Date.now();
          sentRef.current = {
            arriving: st.active_ride.status !== "DRIVER_ASSIGNED",
            arrived: st.active_ride.status === "ARRIVED" || st.active_ride.status === "IN_PROGRESS",
          };
        } else if (st.status === "available") {
          setOnline(true); // was online before the refresh; stay online
        }
      } catch {
        // Best-effort on load; the toggle still works without it.
      } finally {
        if (!stale) setBooting(false);
      }
    })();
    return () => {
      stale = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [persona.id]);

  // Stop any title flash when the panel unmounts.
  useEffect(() => () => stopTitleFlash(), []);

  const goOnline = async (v: boolean) => {
    setBusy("toggle");
    try {
      await api.setAvailability(persona.id, v);
      setOnline(v);
      if (v) {
        await api.ping(persona.id, posRef.current[0], posRef.current[1]).catch(() => {});
        toast.info(`${persona.name} is online`);
      } else {
        toast.info(`${persona.name} is offline`);
      }
    } catch (e) {
      toast.error(e, "Could not change availability");
    } finally {
      setBusy(null);
    }
  };

  // ---- driver offer channel ----
  useEffect(() => {
    if (!online) return;
    const handle = openStream(`/v1/events/driver/${persona.id}`, persona.token, {
      onEvent: (env: SSEEnvelope) => {
        if (env.type === "ride.offer") {
          const d = env.data as OfferData;
          if (rideRef.current) return; // already on a ride
          setOffer((prev) => {
            // Only alert on a genuinely new offer (not a re-delivery of the same).
            if (!prev || prev.ride_id !== d.ride_id) {
              playChirp();
              startTitleFlash(OFFER_TITLE);
            }
            return d;
          });
        }
      },
      onStatus: (s) => setSseStatus("offer", s),
    });
    return () => {
      handle.close();
      setSseStatus("offer", "open");
    };
  }, [online, persona.id, persona.token, setSseStatus]);

  // ---- offer countdown ----
  useEffect(() => {
    if (!offer) return;
    const start = Date.now();
    const expires = new Date(offer.expires_at).getTime();
    const total = Math.max(1000, expires - start);
    const iv = setInterval(() => {
      const left = expires - Date.now();
      if (left <= 0) {
        setOffer(null);
        setCountdown(1);
        stopTitleFlash(); // offer expired
      } else {
        setCountdown(left / total);
      }
    }, 100);
    return () => clearInterval(iv);
  }, [offer]);

  // ---- ride channel (track status once assigned) ----
  useEffect(() => {
    if (!ride) return;
    const handle = openStream(`/v1/events?ride_id=${ride.id}`, persona.token, {
      onEvent: (env: SSEEnvelope) => {
        if (env.type === "ride.status_changed") {
          const st = (env.data as StatusChangedData).status;
          setStatus(st);
          if (st === "IN_PROGRESS" && tripStartRef.current == null) tripStartRef.current = Date.now();
          if (st === "CANCELLED_BY_RIDER" || st === "EXPIRED") {
            toast.info("Ride was cancelled by rider");
            resetRide();
          }
        }
      },
      onStatus: (s) => setSseStatus("ride", s),
    });
    return () => {
      handle.close();
      setSseStatus("ride", "open");
    };
  }, [ride?.id, persona.token]); // eslint-disable-line react-hooks/exhaustive-deps

  // ---- compute the driving leg (OSRM road route) whenever the leg changes ----
  useEffect(() => {
    if (!ride || (phase !== "to_pickup" && phase !== "to_drop")) {
      legRef.current = null;
      setLegPath(null);
      setEta(null);
      return;
    }
    const from: LatLng = phase === "to_pickup" ? posRef.current : [ride.pickup_lat, ride.pickup_lng];
    const to: LatLng = phase === "to_pickup" ? [ride.pickup_lat, ride.pickup_lng] : [ride.drop_lat, ride.drop_lng];

    // Immediate local fallback so the car never stalls waiting on the network.
    const fallback = buildRoute(from, to);
    legRef.current = makeLeg(phase, fallback, 0, CITY_SPEED_MPS);
    setLegPath(fallback);

    let cancelled = false;
    fetchRoad(from, to).then((road) => {
      if (cancelled || !road) return; // OSRM unreachable → keep the fallback
      const cur = legRef.current;
      if (!cur || cur.phase !== phase) return; // moved on to another leg
      const speed = road.distanceM > 0 && road.durationS > 0 ? road.distanceM / road.durationS : CITY_SPEED_MPS;
      legRef.current = makeLeg(phase, road.path, cur.driven, speed);
      setLegPath(road.path);
    });
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ride?.id, phase]);

  const accept = async () => {
    if (!offer) return;
    setBusy("accept");
    try {
      const r = await api.accept(persona.id, offer.ride_id);
      setRide(r);
      setStatus(r.status);
      setOffer(null);
      stopTitleFlash();
      sentRef.current = { arriving: false, arrived: false };
      toast.success("Ride accepted");
    } catch (e) {
      toast.error(e, "Could not accept");
      setOffer(null);
      stopTitleFlash();
    } finally {
      setBusy(null);
    }
  };

  const decline = async () => {
    setConfirm(null);
    if (!offer) return;
    setBusy("decline");
    try {
      await api.decline(persona.id, offer.ride_id);
    } catch {
      /* offer may have expired */
    } finally {
      setBusy(null);
    }
    setOffer(null);
    stopTitleFlash();
  };

  const doArriving = async () => {
    if (!ride) return;
    setBusy("arriving");
    try {
      await api.arriving(ride.id);
    } catch (e) {
      toast.error(e);
    } finally {
      setBusy(null);
    }
  };
  const doArrived = async () => {
    if (!ride) return;
    setBusy("arrived");
    try {
      await api.arrived(ride.id);
    } catch (e) {
      toast.error(e);
    } finally {
      setBusy(null);
    }
  };

  const startTrip = async () => {
    if (!ride) return;
    setBusy("start");
    try {
      await api.startTrip(ride.id, otpInput.trim());
      setOtpInput("");
      tripStartRef.current = Date.now();
      toast.success("Trip started");
    } catch (e) {
      toast.error(e, "Invalid OTP");
    } finally {
      setBusy(null);
    }
  };

  const endTrip = async () => {
    setConfirm(null);
    if (!ride) return;
    setBusy("end");
    try {
      const t = await api.endTrip(ride.id);
      toast.success(`Trip complete · earned ${rupees(t.fare?.total ?? 0)}`);
      resetRide();
    } catch (e) {
      toast.error(e, "Could not end trip");
    } finally {
      setBusy(null);
    }
  };

  const resetRide = useCallback(() => {
    setRide(null);
    setStatus(null);
    setOtpInput("");
    tripStartRef.current = null;
    legRef.current = null;
    setLegPath(null);
    setEta(null);
    sentRef.current = { arriving: false, arrived: false };
  }, []);

  // ---- 60fps motion loop (renders the car gliding along the polyline) ----
  // Render motion is decoupled from the network tick below: this rAF loop only
  // advances the car and rotates it; it never touches the network.
  useEffect(() => {
    if (!online) return;
    let raf = 0;
    let last = performance.now();
    const tick = (now: number) => {
      let dt = (now - last) / 1000;
      last = now;
      if (dt > 1.5) dt = 1.5; // clamp: don't teleport after a backgrounded tab

      const ph = phaseFor(statusRef.current);
      const leg = legRef.current;
      // Motion is paused for idle / at_pickup — only drive on the two legs.
      if ((ph === "to_pickup" || ph === "to_drop") && leg && leg.phase === ph && leg.pts.length > 1) {
        leg.driven = Math.min(leg.cumTotal, leg.driven + DRIVE_SPEED_M * dt);
        const { pos: next, brg } = pointAtDistanceCum(leg.pts, leg.cum, leg.driven);
        posRef.current = next;
        setPos(next);
        setCarBrg(brg);
      }
      raf = requestAnimationFrame(tick);
    };
    raf = requestAnimationFrame(tick);
    // Resume cleanly when the tab returns — reset the clock so the clamped dt
    // above never produces a jump.
    const onVis = () => {
      if (!document.hidden) last = performance.now();
    };
    document.addEventListener("visibilitychange", onVis);
    return () => {
      cancelAnimationFrame(raf);
      document.removeEventListener("visibilitychange", onVis);
    };
  }, [online]);

  // ---- 1s network tick (location ping + autopilot arriving/arrived + ETA) ----
  useEffect(() => {
    if (!online) return;
    const iv = setInterval(() => {
      const r = rideRef.current;
      const st = statusRef.current;
      const ph = phaseFor(st);
      const leg = legRef.current;

      if (r && ph === "to_pickup" && leg && leg.phase === ph && leg.pts.length > 1) {
        const remaining = Math.max(0, leg.cumTotal - leg.driven);
        setEta(Math.round(remaining / (leg.speedMps || CITY_SPEED_MPS)));
        if (autopilotRef.current) {
          const dp = haversine(posRef.current, [r.pickup_lat, r.pickup_lng]);
          if (dp < NEAR_PICKUP_M * 1.6 && st === "DRIVER_ASSIGNED" && !sentRef.current.arriving) {
            sentRef.current.arriving = true;
            api.arriving(r.id).catch(() => {});
          }
          if (dp < NEAR_PICKUP_M && (st === "DRIVER_ARRIVING" || st === "DRIVER_ASSIGNED") && !sentRef.current.arrived) {
            sentRef.current.arrived = true;
            setTimeout(() => api.arrived(r.id).catch(() => {}), 1000);
          }
        }
      }
      // Always ping (keeps us fresh in the geo index / feeds rider tracking).
      api.ping(persona.id, posRef.current[0], posRef.current[1]).catch(() => {});
    }, 1000);
    return () => clearInterval(iv);
  }, [online, api, persona.id]);

  // ---- map props ----
  const route = ride ? legPath : null;

  // Fit to the leg geometry (origin + pickup + drop), NOT the live car position
  // — the car glides at 60fps and would otherwise re-fit the map every frame.
  // Keeping the car on-screen is CarFollow's job (throttled panInside).
  const fitPoints = useMemo<LatLng[]>(() => {
    if (!ride) return [pos];
    const pts: LatLng[] = [
      [ride.pickup_lat, ride.pickup_lng],
      [ride.drop_lat, ride.drop_lng],
    ];
    if (legPath && legPath.length > 0) pts.push(legPath[0]);
    return pts;
  }, [pos, ride, legPath]);

  const offerKm = offer ? (haversine(pos, [offer.pickup_lat, offer.pickup_lng]) / 1000).toFixed(1) : "0";

  // Live metered km / duration surfaced in the End-trip confirm.
  const meteredKm = phase === "to_drop" && legRef.current ? (legRef.current.driven / 1000).toFixed(1) : "0.0";
  const meteredMin = tripStartRef.current ? Math.max(1, Math.round((Date.now() - tripStartRef.current) / 60000)) : 0;

  return (
    <div className="panel">
      <div className="panel-map">
        <MapView
          center={pos}
          pickup={ride ? [ride.pickup_lat, ride.pickup_lng] : null}
          drop={ride ? [ride.drop_lat, ride.drop_lng] : null}
          route={route}
          car={pos}
          carBearing={carBrg}
          animateCar={false}
          bots={bots}
          fitPoints={fitPoints}
        />
      </div>

      <div className="persona-chip driver">
        <div className="avatar">{persona.name.charAt(0)}</div>
        <div>
          <span className="role">Driver</span>
          <span className="name">{persona.name}</span>
        </div>
      </div>

      {reconnecting && (
        <div className="sse-pill" role="status">
          <span className="spinner" aria-hidden="true" />
          Reconnecting…
        </div>
      )}

      <label className="persona-select">
        <select
          aria-label="Select driver persona"
          value={persona.id}
          onChange={(e) => onPersonaChange(e.target.value)}
          disabled={online}
        >
          {DRIVERS.map((d) => (
            <option key={d.id} value={d.id}>
              {d.name} · {d.tier}
            </option>
          ))}
        </select>
      </label>

      {offer && (
        <div className="offer-modal pulse" role="dialog" aria-modal="true" aria-label="New ride request">
          <div className="offer-progress">
            <span style={{ width: `${Math.max(0, countdown * 100)}%` }} />
          </div>
          <div className="offer-top">
            <span className="offer-tag">● New ride request</span>
            <span className="offer-headline">{rupees(offer.fare)}</span>
          </div>
          <div className="offer-route">
            {placeName(offer.pickup_lat, offer.pickup_lng)} → {placeName(offer.drop_lat, offer.drop_lng)}
          </div>
          <div className="offer-meta">
            {(offer.distance_m / 1000).toFixed(1)} km · {Math.round(offer.duration_s / 60)} min ·{" "}
            {offer.rider_name} ★ {offer.rider_rating.toFixed(1)} · {offerKm} km to pickup
          </div>
          <div className="offer-actions">
            <button className="btn go" onClick={accept} disabled={busy !== null}>
              {busy === "accept" ? <Spinner label="Accepting…" /> : "Accept"}
            </button>
            <button className="btn dark" onClick={() => setConfirm("decline")} disabled={busy !== null}>
              {busy === "decline" ? <Spinner /> : "Decline"}
            </button>
          </div>
        </div>
      )}

      <div className={`float-card bottom-right ${collapsed ? "collapsed" : ""}`}>
        {!booting && (
          <button
            type="button"
            className="card-min"
            aria-label={collapsed ? "Expand" : "Minimize"}
            aria-expanded={!collapsed}
            onClick={() => setCollapsed((c) => !c)}
          >
            <ChevronIcon />
          </button>
        )}
        {booting ? (
          <PanelShimmer />
        ) : collapsed ? (
          <DriverCollapsed
            ride={ride}
            phase={phase}
            online={online}
            eta={eta}
            busy={busy}
            onEnd={() => setConfirm("end")}
          />
        ) : (
          <div className="card-body">
            {!ride ? (
              <OnlineView
                online={online}
                busy={busy}
                onToggle={goOnline}
                persona={persona}
                canJump={!!lastRiderPickup && !online}
                onJumpToPickup={() => lastRiderPickup && setPos(lastRiderPickup)}
                autopilot={autopilot}
                setAutopilot={setAutopilot}
              />
            ) : (
              <ActiveDriveView
                ride={ride}
                status={status}
                phase={phase}
                otpInput={otpInput}
                setOtpInput={setOtpInput}
                busy={busy}
                autopilot={autopilot}
                eta={eta}
                onArriving={doArriving}
                onArrived={doArrived}
                onStart={startTrip}
                onEnd={() => setConfirm("end")}
              />
            )}
          </div>
        )}
      </div>

      {confirm === "decline" && (
        <ConfirmDialog
          title="Decline this request?"
          message="The ride will be offered to another nearby driver."
          confirmLabel="Decline"
          cancelLabel="Keep"
          tone="danger"
          busy={busy === "decline"}
          onConfirm={decline}
          onCancel={() => setConfirm(null)}
        />
      )}

      {confirm === "end" && (
        <ConfirmDialog
          title="End trip?"
          message="This finalises the fare and completes the ride."
          confirmLabel="End trip"
          cancelLabel="Keep driving"
          tone="danger"
          busy={busy === "end"}
          onConfirm={endTrip}
          onCancel={() => setConfirm(null)}
        >
          <div className="metered">
            <div>
              <div className="m-val">{meteredKm} km</div>
              <div className="m-lbl">Metered distance</div>
            </div>
            <div>
              <div className="m-val">{meteredMin || "—"} min</div>
              <div className="m-lbl">Trip duration</div>
            </div>
          </div>
        </ConfirmDialog>
      )}
    </div>
  );
}

function PanelShimmer() {
  return (
    <div className="shimmer-block" aria-busy="true" aria-label="Loading">
      <div className="sk sk-title" />
      <div className="sk sk-line" />
      <div className="sk sk-row" />
      <div className="sk sk-btn" />
    </div>
  );
}

function ChevronIcon() {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M6 9l6 6 6-6" />
    </svg>
  );
}

// One-line summary shown when the driver's card is minimized, keeping the map
// unobstructed while preserving the single most important action (End trip).
function DriverCollapsed({
  ride,
  phase,
  online,
  eta,
  busy,
  onEnd,
}: {
  ride: RideView | null;
  phase: Phase;
  online: boolean;
  eta: number | null;
  busy: string | null;
  onEnd: () => void;
}) {
  if (!ride) {
    return (
      <div className="collapse-summary">
        <span className="cs-text">
          {online ? "You're online" : "You're offline"}
          <span className="cs-dim">{online ? " · waiting for offers" : ""}</span>
        </span>
      </div>
    );
  }
  const fare = ride.fare_total != null ? rupees(ride.fare_total) : "—";
  if (phase === "to_drop") {
    return (
      <div className="collapse-summary">
        <span className="cs-text">
          Trip in progress <span className="cs-dim">· {fare}</span>
        </span>
        <button className="btn primary" onClick={onEnd} disabled={busy !== null}>
          {busy === "end" ? <Spinner label="Ending…" /> : "End trip"}
        </button>
      </div>
    );
  }
  const etaMin = eta != null ? Math.max(1, Math.round(eta / 60)) : null;
  const label = phase === "at_pickup" ? "Verify OTP to start" : phase === "to_pickup" ? "Head to pickup" : "Active ride";
  return (
    <div className="collapse-summary">
      <span className="cs-text">
        {label}
        {etaMin != null && phase === "to_pickup" && <span className="cs-dim"> · ~{etaMin} min</span>}
      </span>
    </div>
  );
}

function OnlineView({
  online,
  busy,
  onToggle,
  persona,
  canJump,
  onJumpToPickup,
  autopilot,
  setAutopilot,
}: {
  online: boolean;
  busy: string | null;
  onToggle: (v: boolean) => void;
  persona: DriverPersona;
  canJump: boolean;
  onJumpToPickup: () => void;
  autopilot: boolean;
  setAutopilot: (v: boolean) => void;
}) {
  return (
    <>
      <div className="row" style={{ justifyContent: "space-between", marginBottom: 12 }}>
        <div>
          <h3>{online ? "You're online" : "You're offline"}</h3>
          <p className="sub" style={{ margin: 0 }}>
            {online ? "Waiting for ride offers…" : "Go online to receive offers"}
          </p>
        </div>
        <button
          type="button"
          role="switch"
          aria-checked={online}
          aria-label={online ? "Go offline" : "Go online"}
          aria-busy={busy === "toggle"}
          className={`toggle ${online ? "on" : ""} ${busy === "toggle" ? "busy" : ""}`}
          disabled={busy === "toggle"}
          onClick={() => onToggle(!online)}
        >
          <div className="knob" />
        </button>
      </div>

      <div className="vehicle-row">
        <div className="veh-tile">🚗</div>
        <div className="info">
          <div className="n">
            {persona.vehicleModel} <span className="tier-tag">· {persona.tier}</span>
          </div>
          <span className="plate">{persona.plate}</span>
        </div>
        <div className="rating">★ {persona.rating.toFixed(1)}</div>
      </div>

      {canJump && (
        <button className="btn dark" onClick={onJumpToPickup} style={{ marginBottom: 10 }}>
          📍 Position at rider's pickup
        </button>
      )}
      <p className="muted" style={{ marginBottom: 12 }}>
        Tip: position near the rider's pickup before going online so you're the nearest match.
      </p>

      <div className="opt-row">
        <div>
          <div className="opt-title">Auto-pilot</div>
          <div className="opt-desc">Auto-drive to pickup and mark arriving/arrived</div>
        </div>
        <button
          type="button"
          role="switch"
          aria-checked={autopilot}
          aria-label="Auto-pilot"
          className={`toggle ${autopilot ? "on" : ""}`}
          onClick={() => setAutopilot(!autopilot)}
        >
          <div className="knob" />
        </button>
      </div>
    </>
  );
}

function ActiveDriveView({
  ride,
  status,
  phase,
  otpInput,
  setOtpInput,
  busy,
  autopilot,
  eta,
  onArriving,
  onArrived,
  onStart,
  onEnd,
}: {
  ride: RideView;
  status: RideStatus | null;
  phase: Phase;
  otpInput: string;
  setOtpInput: (v: string) => void;
  busy: string | null;
  autopilot: boolean;
  eta: number | null;
  onArriving: () => void;
  onArrived: () => void;
  onStart: () => void;
  onEnd: () => void;
}) {
  const etaMin = eta != null ? Math.max(1, Math.round(eta / 60)) : null;
  return (
    <>
      <h3>
        {phase === "to_pickup"
          ? "Head to pickup"
          : phase === "at_pickup"
            ? "Verify OTP to start"
            : phase === "to_drop"
              ? "Trip in progress"
              : "Active ride"}
      </h3>
      <p className="sub">
        {ride.tier.toUpperCase()} · fare {ride.fare_total != null ? rupees(ride.fare_total) : "—"} · via{" "}
        {ride.payment_method?.toUpperCase()}
      </p>

      {phase === "to_pickup" && (
        <>
          {etaMin != null && (
            <div className="eta-chip">
              🚗 ~{etaMin} min away
            </div>
          )}
          {autopilot ? (
            <p className="muted">Auto-pilot is driving you to the pickup…</p>
          ) : (
            <div className="row" style={{ gap: 10 }}>
              <button className="btn dark" onClick={onArriving} disabled={busy !== null || status !== "DRIVER_ASSIGNED"}>
                {busy === "arriving" ? <Spinner /> : "Arriving"}
              </button>
              <button className="btn dark" onClick={onArrived} disabled={busy !== null || status !== "DRIVER_ARRIVING"}>
                {busy === "arrived" ? <Spinner /> : "Arrived"}
              </button>
            </div>
          )}
        </>
      )}

      {phase === "at_pickup" && (
        <>
          <div className="field-label">Enter rider's 4-digit OTP</div>
          <input
            className="otp-input"
            value={otpInput}
            onChange={(e) => setOtpInput(e.target.value.replace(/\D/g, "").slice(0, 4))}
            placeholder="0000"
            inputMode="numeric"
            maxLength={4}
          />
          <button className="btn go" onClick={onStart} disabled={busy !== null || otpInput.length !== 4}>
            {busy === "start" ? <Spinner label="Verifying…" /> : "Start trip"}
          </button>
        </>
      )}

      {phase === "to_drop" && (
        <button className="btn primary" onClick={onEnd} disabled={busy !== null}>
          {busy === "end" ? <Spinner label="Ending…" /> : "End trip"}
        </button>
      )}
    </>
  );
}
