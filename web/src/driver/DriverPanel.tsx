import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Api } from "../api/client";
import type { OfferData, RideStatus, RideView, SSEEnvelope, StatusChangedData } from "../api/types";
import type { DriverPersona } from "../config/personas";
import { DRIVERS } from "../config/personas";
import { buildRoute, haversine, lerp, type LatLng } from "../lib/geo";
import { rupees } from "../lib/money";
import { MapView, type BotMarker } from "../map/MapView";
import { openStream } from "../sse/stream";
import { useToast } from "../ui/toast";

const DRIVE_SPEED_M = 200; // metres per ~1s tick
const NEAR_PICKUP_M = 90;

interface Props {
  persona: DriverPersona;
  onPersonaChange: (id: string) => void;
  lastRiderPickup: LatLng | null;
  bots: BotMarker[];
}

type Phase = "idle" | "to_pickup" | "at_pickup" | "to_drop";

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
  const [ride, setRide] = useState<RideView | null>(null);
  const [status, setStatus] = useState<RideStatus | null>(null);
  const [offer, setOffer] = useState<OfferData | null>(null);
  const [countdown, setCountdown] = useState(1);
  const [otpInput, setOtpInput] = useState("");
  const [autopilot, setAutopilot] = useState(true);
  const [busy, setBusy] = useState(false);

  // Refs the 1s driving loop reads (avoids stale closures).
  const posRef = useRef(pos);
  const rideRef = useRef<RideView | null>(null);
  const statusRef = useRef<RideStatus | null>(null);
  const autopilotRef = useRef(autopilot);
  const sentRef = useRef<{ arriving: boolean; arrived: boolean }>({ arriving: false, arrived: false });

  useEffect(() => { posRef.current = pos; }, [pos]);
  useEffect(() => { rideRef.current = ride; }, [ride]);
  useEffect(() => { statusRef.current = status; }, [status]);
  useEffect(() => { autopilotRef.current = autopilot; }, [autopilot]);

  const phase = phaseFor(status);

  // Reset on persona switch.
  useEffect(() => {
    setOnline(false);
    setRide(null);
    setStatus(null);
    setOffer(null);
    setOtpInput("");
  }, [persona.id]);

  const goOnline = async (v: boolean) => {
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
    }
  };

  // ---- driver offer channel ----
  useEffect(() => {
    if (!online) return;
    const handle = openStream(`/v1/events/driver/${persona.id}`, persona.token, {
      onEvent: (env: SSEEnvelope) => {
        if (env.type === "ride.offer") {
          const d = env.data as OfferData;
          // Ignore offers while already on a ride.
          if (rideRef.current) return;
          setOffer(d);
        }
      },
    });
    return () => handle.close();
  }, [online, persona.id, persona.token]);

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
          if (st === "CANCELLED_BY_RIDER" || st === "EXPIRED") {
            toast.info("Ride was cancelled by rider");
            resetRide();
          }
        }
      },
    });
    return () => handle.close();
  }, [ride?.id, persona.token]); // eslint-disable-line react-hooks/exhaustive-deps

  const accept = async () => {
    if (!offer) return;
    setBusy(true);
    try {
      const r = await api.accept(persona.id, offer.ride_id);
      setRide(r);
      setStatus(r.status);
      setOffer(null);
      sentRef.current = { arriving: false, arrived: false };
      toast.success("Ride accepted");
    } catch (e) {
      toast.error(e, "Could not accept");
      setOffer(null);
    } finally {
      setBusy(false);
    }
  };

  const decline = async () => {
    if (!offer) return;
    try {
      await api.decline(persona.id, offer.ride_id);
    } catch {
      /* offer may have expired */
    }
    setOffer(null);
  };

  const doArriving = async () => {
    if (!ride) return;
    try {
      await api.arriving(ride.id);
    } catch (e) {
      toast.error(e);
    }
  };
  const doArrived = async () => {
    if (!ride) return;
    try {
      await api.arrived(ride.id);
    } catch (e) {
      toast.error(e);
    }
  };

  const startTrip = async () => {
    if (!ride) return;
    setBusy(true);
    try {
      await api.startTrip(ride.id, otpInput.trim());
      setOtpInput("");
      toast.success("Trip started");
    } catch (e) {
      toast.error(e, "Invalid OTP");
    } finally {
      setBusy(false);
    }
  };

  const endTrip = async () => {
    if (!ride) return;
    setBusy(true);
    try {
      const t = await api.endTrip(ride.id);
      toast.success(`Trip complete · earned ${rupees(t.fare?.total ?? 0)}`);
      resetRide();
    } catch (e) {
      toast.error(e, "Could not end trip");
    } finally {
      setBusy(false);
    }
  };

  const resetRide = useCallback(() => {
    setRide(null);
    setStatus(null);
    setOtpInput("");
    sentRef.current = { arriving: false, arrived: false };
  }, []);

  // ---- 1s driving + ping loop ----
  useEffect(() => {
    if (!online) return;
    const iv = setInterval(() => {
      const r = rideRef.current;
      const st = statusRef.current;
      const ph = phaseFor(st);
      let cur = posRef.current;

      // Determine target.
      let target: LatLng | null = null;
      if (r && ph === "to_pickup") target = [r.pickup_lat, r.pickup_lng];
      else if (r && ph === "to_drop") target = [r.drop_lat, r.drop_lng];

      if (target) {
        const d = haversine(cur, target);
        cur = d < DRIVE_SPEED_M ? target : lerp(cur, target, DRIVE_SPEED_M / d);
        posRef.current = cur;
        setPos(cur);

        // Auto-progress arriving/arrived near pickup.
        if (autopilotRef.current && ph === "to_pickup" && r) {
          const dp = haversine(cur, [r.pickup_lat, r.pickup_lng]);
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
  const route = useMemo<LatLng[] | null>(() => {
    if (!ride) return null;
    if (phase === "to_pickup") return buildRoute(pos, [ride.pickup_lat, ride.pickup_lng]);
    return buildRoute([ride.pickup_lat, ride.pickup_lng], [ride.drop_lat, ride.drop_lng]);
  }, [ride, phase, pos]);

  const fitPoints = useMemo<LatLng[]>(() => {
    const pts: LatLng[] = [pos];
    if (ride) pts.push([ride.pickup_lat, ride.pickup_lng], [ride.drop_lat, ride.drop_lng]);
    return pts;
  }, [pos, ride]);

  return (
    <div className="phone">
      <div className="phone-topbar">
        <div>
          <div className="role">Driver</div>
          <div className="who">{persona.name}</div>
        </div>
        <select className="select" value={persona.id} onChange={(e) => onPersonaChange(e.target.value)} disabled={online}>
          {DRIVERS.map((d) => (
            <option key={d.id} value={d.id}>
              {d.name} · {d.tier}
            </option>
          ))}
        </select>
      </div>

      <div className="phone-map">
        <MapView
          center={pos}
          pickup={ride ? [ride.pickup_lat, ride.pickup_lng] : null}
          drop={ride ? [ride.drop_lat, ride.drop_lng] : null}
          route={route}
          car={pos}
          bots={bots}
          fitPoints={fitPoints}
        />

        {offer && (
          <div className="offer-overlay">
            <div className="offer-card">
              <div className="role">New ride offer</div>
              <h2>{offer.tier.toUpperCase()} trip</h2>
              <div className="countdown">
                <div className="bar" style={{ width: `${Math.max(0, countdown * 100)}%` }} />
              </div>
              <div className="offer-stats">
                <div className="offer-stat">
                  <div className="v">{(haversine(pos, [offer.pickup_lat, offer.pickup_lng]) / 1000).toFixed(1)} km</div>
                  <div className="l">to pickup</div>
                </div>
                <div className="offer-stat">
                  <div className="v">{offer.tier}</div>
                  <div className="l">vehicle tier</div>
                </div>
              </div>
              <div className="row" style={{ gap: 10 }}>
                <button className="btn dark" onClick={decline}>
                  Decline
                </button>
                <button className="btn go" onClick={accept} disabled={busy}>
                  Accept
                </button>
              </div>
            </div>
          </div>
        )}
      </div>

      <div className="sheet">
        {!ride ? (
          <OnlineView
            online={online}
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
            onArriving={doArriving}
            onArrived={doArrived}
            onStart={startTrip}
            onEnd={endTrip}
          />
        )}
      </div>
    </div>
  );
}

function OnlineView({
  online,
  onToggle,
  persona,
  canJump,
  onJumpToPickup,
  autopilot,
  setAutopilot,
}: {
  online: boolean;
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
          className={`toggle ${online ? "on" : ""}`}
          onClick={() => onToggle(!online)}
        >
          <div className="knob" />
        </button>
      </div>

      <div className="dcard">
        <div className="avatar">{persona.name.charAt(0)}</div>
        <div className="info">
          <div className="n">
            {persona.vehicleModel} · <span style={{ textTransform: "capitalize" }}>{persona.tier}</span>
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

      <div className="row" style={{ justifyContent: "space-between" }}>
        <div>
          <div style={{ fontWeight: 600 }}>Auto-pilot</div>
          <div className="muted">Auto-drive to pickup and mark arriving/arrived</div>
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
  busy: boolean;
  autopilot: boolean;
  onArriving: () => void;
  onArrived: () => void;
  onStart: () => void;
  onEnd: () => void;
}) {
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
          {autopilot ? (
            <p className="muted">Auto-pilot is driving you to the pickup…</p>
          ) : (
            <div className="row" style={{ gap: 10 }}>
              <button className="btn dark" onClick={onArriving} disabled={status !== "DRIVER_ASSIGNED"}>
                Arriving
              </button>
              <button className="btn dark" onClick={onArrived} disabled={status !== "DRIVER_ARRIVING"}>
                Arrived
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
          <button className="btn go" onClick={onStart} disabled={busy || otpInput.length !== 4}>
            {busy ? "Verifying…" : "Start trip"}
          </button>
        </>
      )}

      {phase === "to_drop" && (
        <button className="btn primary" onClick={onEnd} disabled={busy}>
          {busy ? "Ending…" : "End trip"}
        </button>
      )}
    </>
  );
}
