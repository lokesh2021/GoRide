import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Api } from "../api/client";
import type {
  DriverLocationData,
  HistoryItem,
  OtpData,
  PaymentMethod,
  PaymentUpdatedData,
  QuoteResponse,
  RideStatus,
  RideView,
  SSEEnvelope,
  StatusChangedData,
  Tier,
} from "../api/types";
import type { RiderPersona } from "../config/personas";
import { PLACES, RIDERS } from "../config/personas";
import { bearing, buildRoute, formatDuration, formatKm, type LatLng } from "../lib/geo";
import { rupees, surgeLabel } from "../lib/money";
import { MapView, type BotMarker } from "../map/MapView";
import { openStream } from "../sse/stream";
import { useToast } from "../ui/toast";

const TIER_META: Record<Tier, { icon: string; label: string; seats: string }> = {
  mini: { icon: "🚗", label: "Mini", seats: "4 seats" },
  sedan: { icon: "🚙", label: "Sedan", seats: "4 seats" },
  xl: { icon: "🚐", label: "XL", seats: "6 seats" },
};

const STATUS_RANK: Record<string, number> = {
  DRIVER_ASSIGNED: 1,
  DRIVER_ARRIVING: 2,
  ARRIVED: 3,
  IN_PROGRESS: 4,
  COMPLETED: 5,
};

function canCancel(s: RideStatus): boolean {
  return ["REQUESTED", "MATCHING", "DRIVER_ASSIGNED", "DRIVER_ARRIVING", "ARRIVED"].includes(s);
}

interface Props {
  persona: RiderPersona;
  onPersonaChange: (id: string) => void;
  onPickupChange: (p: LatLng | null) => void;
  bots: BotMarker[];
}

export function RiderPanel({ persona, onPersonaChange, onPickupChange, bots }: Props) {
  const toast = useToast();
  const api = useMemo(() => new Api(persona.token), [persona.token]);

  const [pickup, setPickup] = useState<LatLng | null>([12.9756, 77.6068]); // MG Road
  const [drop, setDrop] = useState<LatLng | null>([12.9352, 77.6245]); // Koramangala
  const [picking, setPicking] = useState<"pickup" | "drop" | null>(null);

  const [quote, setQuote] = useState<QuoteResponse | null>(null);
  const [tier, setTier] = useState<Tier>("mini");
  const [method, setMethod] = useState<PaymentMethod>("upi");
  const [busy, setBusy] = useState(false);

  const [ride, setRide] = useState<RideView | null>(null);
  const [status, setStatus] = useState<RideStatus | null>(null);
  const [otp, setOtp] = useState<string | null>(null);
  const [fare, setFare] = useState<StatusChangedData["fare"] | null>(null);
  const [paymentStatus, setPaymentStatus] = useState<string | null>(null);
  const [driverPos, setDriverPos] = useState<LatLng | null>(null);
  const [driverBrg, setDriverBrg] = useState(0);

  const [showHistory, setShowHistory] = useState(false);
  const [history, setHistory] = useState<HistoryItem[]>([]);

  const prevDriverPos = useRef<LatLng | null>(null);

  useEffect(() => onPickupChange(pickup), [pickup, onPickupChange]);

  // Reset everything when the persona switches.
  useEffect(() => {
    setRide(null);
    setStatus(null);
    setQuote(null);
    setOtp(null);
    setFare(null);
    setPaymentStatus(null);
    setDriverPos(null);
    setShowHistory(false);
  }, [persona.id]);

  // ---- SSE: subscribe to the ride channel while a ride is live ----
  useEffect(() => {
    if (!ride) return;
    const handle = openStream(`/v1/events?ride_id=${ride.id}`, persona.token, {
      onEvent: (env: SSEEnvelope) => {
        switch (env.type) {
          case "ride.status_changed": {
            const d = env.data as StatusChangedData;
            setStatus(d.status);
            if (d.driver) setRide((r) => (r ? { ...r, driver: d.driver, driver_id: r.driver_id ?? "assigned" } : r));
            if (d.fare) setFare(d.fare);
            break;
          }
          case "ride.otp":
            setOtp((env.data as OtpData).otp);
            break;
          case "ride.driver_location": {
            const d = env.data as DriverLocationData;
            const pos: LatLng = [d.lat, d.lng];
            if (prevDriverPos.current) setDriverBrg(bearing(prevDriverPos.current, pos));
            prevDriverPos.current = pos;
            setDriverPos(pos);
            break;
          }
          case "payment.updated": {
            const d = env.data as PaymentUpdatedData;
            setPaymentStatus(d.status);
            if (d.status === "SUCCEEDED") toast.success("Payment successful");
            if (d.status === "FAILED") toast.error(new Error("Payment failed — you can retry"));
            break;
          }
        }
      },
    });
    return () => handle.close();
  }, [ride?.id, persona.token]); // eslint-disable-line react-hooks/exhaustive-deps

  const getFares = async () => {
    if (!pickup || !drop) {
      toast.info("Set both pickup and destination");
      return;
    }
    setBusy(true);
    try {
      const q = await api.quote({ pickup: { lat: pickup[0], lng: pickup[1] }, drop: { lat: drop[0], lng: drop[1] } });
      setQuote(q);
    } catch (e) {
      toast.error(e, "Could not fetch fares");
    } finally {
      setBusy(false);
    }
  };

  const book = async () => {
    if (!quote) return;
    setBusy(true);
    try {
      const r = await api.createRide({ quote_id: quote.quote_id, tier, payment_method: method });
      setRide(r);
      setStatus(r.status);
      setQuote(null);
    } catch (e) {
      toast.error(e, "Booking failed");
    } finally {
      setBusy(false);
    }
  };

  const cancel = async () => {
    if (!ride) return;
    try {
      await api.cancelRide(ride.id, "changed my mind");
      toast.info("Ride cancelled");
      resetToPlan();
    } catch (e) {
      toast.error(e, "Cancel failed");
    }
  };

  const pay = async () => {
    if (!ride) return;
    setBusy(true);
    try {
      const p = await api.pay(ride.id);
      setPaymentStatus(p.status);
      toast.info("Payment processing…");
    } catch (e) {
      toast.error(e, "Payment failed");
    } finally {
      setBusy(false);
    }
  };

  const loadHistory = async () => {
    try {
      const h = await api.history(persona.id);
      setHistory(h.rides);
      setShowHistory(true);
    } catch (e) {
      toast.error(e, "Could not load history");
    }
  };

  const resetToPlan = useCallback(() => {
    setRide(null);
    setStatus(null);
    setOtp(null);
    setFare(null);
    setPaymentStatus(null);
    setDriverPos(null);
    prevDriverPos.current = null;
  }, []);

  // ---- derived map props ----
  const route = useMemo<LatLng[] | null>(() => (pickup && drop ? buildRoute(pickup, drop) : null), [pickup, drop]);
  const fitPoints = useMemo<LatLng[]>(() => {
    const pts: LatLng[] = [];
    if (pickup) pts.push(pickup);
    if (drop) pts.push(drop);
    if (driverPos) pts.push(driverPos);
    return pts;
  }, [pickup, drop, driverPos]);

  return (
    <div className="panel">
      <div className="panel-map">
        <MapView
          center={pickup ?? [12.9716, 77.5946]}
          pickup={pickup}
          drop={drop}
          route={ride && status && STATUS_RANK[status] >= 1 ? route : quote ? route : ride ? null : null}
          car={driverPos}
          carBearing={driverBrg}
          bots={bots}
          picking={ride ? null : picking}
          onPick={(p) => {
            if (picking === "pickup") setPickup(p);
            else if (picking === "drop") setDrop(p);
            setPicking(null);
          }}
          fitPoints={fitPoints}
        />
      </div>

      <div className="persona-chip rider">
        <div className="avatar">{persona.name.charAt(0)}</div>
        <div>
          <span className="role">Rider</span>
          <span className="name">{persona.name}</span>
        </div>
      </div>

      <label className="persona-select">
        <select
          aria-label="Select rider persona"
          value={persona.id}
          onChange={(e) => onPersonaChange(e.target.value)}
          disabled={!!ride && status !== "COMPLETED"}
        >
          {RIDERS.map((r) => (
            <option key={r.id} value={r.id}>
              {r.name}
            </option>
          ))}
        </select>
      </label>

      <div className="float-card bottom-left">
        {showHistory ? (
          <HistoryView items={history} onBack={() => setShowHistory(false)} />
        ) : !ride ? (
          <PlanView
            pickup={pickup}
            drop={drop}
            picking={picking}
            setPicking={setPicking}
            setPickup={setPickup}
            setDrop={setDrop}
            quote={quote}
            tier={tier}
            setTier={setTier}
            method={method}
            setMethod={setMethod}
            busy={busy}
            onGetFares={getFares}
            onBook={book}
            onHistory={loadHistory}
          />
        ) : status === "EXPIRED" || status === "CANCELLED_BY_RIDER" || status === "CANCELLED_BY_DRIVER" ? (
          <TerminalView status={status!} onDone={resetToPlan} />
        ) : status === "COMPLETED" ? (
          <CompletedView
            ride={ride}
            fare={fare}
            paymentStatus={paymentStatus}
            busy={busy}
            onPay={pay}
            onDone={resetToPlan}
            onHistory={loadHistory}
          />
        ) : status && STATUS_RANK[status] >= 1 ? (
          <ActiveView ride={ride} status={status} otp={otp} onCancel={canCancel(status) ? cancel : undefined} />
        ) : (
          <FindingView onCancel={cancel} />
        )}
      </div>
    </div>
  );
}

// ---- sub-views ----

function PlanView(props: {
  pickup: LatLng | null;
  drop: LatLng | null;
  picking: "pickup" | "drop" | null;
  setPicking: (p: "pickup" | "drop" | null) => void;
  setPickup: (p: LatLng) => void;
  setDrop: (p: LatLng) => void;
  quote: QuoteResponse | null;
  tier: Tier;
  setTier: (t: Tier) => void;
  method: PaymentMethod;
  setMethod: (m: PaymentMethod) => void;
  busy: boolean;
  onGetFares: () => void;
  onBook: () => void;
  onHistory: () => void;
}) {
  const { pickup, drop, picking, setPicking, setPickup, setDrop, quote, tier, setTier, method, setMethod, busy } = props;

  const placeName = (p: LatLng | null) => {
    if (!p) return "Tap map or pick a place";
    const near = PLACES.find((pl) => Math.abs(pl.lat - p[0]) < 0.004 && Math.abs(pl.lng - p[1]) < 0.004);
    return near ? near.name : `${p[0].toFixed(4)}, ${p[1].toFixed(4)}`;
  };

  return (
    <>
      <div className="row" style={{ justifyContent: "space-between" }}>
        <h3>Where to?</h3>
        <button className="link-btn" onClick={props.onHistory}>
          History
        </button>
      </div>

      <div className="leg">
        <div className="leg-row" style={{ outline: picking === "pickup" ? "2px solid var(--accent)" : "none" }}>
          <span className="marker pickup" />
          <div className="txt">
            <div className="c">Pickup</div>
            <div className="t">{placeName(pickup)}</div>
          </div>
          <button className="btn ghost" onClick={() => setPicking(picking === "pickup" ? null : "pickup")}>
            {picking === "pickup" ? "Tap map…" : "Set"}
          </button>
        </div>
        <div className="leg-row" style={{ outline: picking === "drop" ? "2px solid var(--accent)" : "none" }}>
          <span className="marker drop" />
          <div className="txt">
            <div className="c">Destination</div>
            <div className="t">{placeName(drop)}</div>
          </div>
          <button className="btn ghost" onClick={() => setPicking(picking === "drop" ? null : "drop")}>
            {picking === "drop" ? "Tap map…" : "Set"}
          </button>
        </div>
      </div>

      <div className="field-label">Quick places (sets {picking ?? "pickup"})</div>
      <div className="chips" style={{ marginBottom: 14 }}>
        {PLACES.map((pl) => (
          <button
            key={pl.name}
            className="chip"
            onClick={() => {
              const p: LatLng = [pl.lat, pl.lng];
              if (picking === "drop") setDrop(p);
              else setPickup(p);
              setPicking(null);
            }}
          >
            {pl.name}
          </button>
        ))}
      </div>

      {!quote ? (
        <button className="btn primary" onClick={props.onGetFares} disabled={busy || !pickup || !drop}>
          {busy ? "Getting fares…" : "Get fares"}
        </button>
      ) : (
        <>
          <div className="row" style={{ justifyContent: "space-between", marginBottom: 8 }}>
            <div className="muted">
              {formatKm(quote.distance_m)} · {formatDuration(quote.duration_s)}
            </div>
            {quote.surge > 1 && <span className="surge-badge">{surgeLabel(quote.surge)} surge</span>}
          </div>

          {(["mini", "sedan", "xl"] as Tier[]).map((t) => (
            <div key={t} className={`tier ${tier === t ? "sel" : ""}`} onClick={() => setTier(t)}>
              <div className="car">{TIER_META[t].icon}</div>
              <div className="meta">
                <div className="name">{TIER_META[t].label}</div>
                <div className="eta">
                  {TIER_META[t].seats} · {formatDuration(quote.duration_s)} trip
                </div>
              </div>
              <div className="price">
                <div className="amt">{rupees(quote.prices[t])}</div>
                {quote.surge > 1 && <span className="surge-badge">{surgeLabel(quote.surge)}</span>}
              </div>
            </div>
          ))}

          <div className="field-label" style={{ marginTop: 12 }}>
            Payment method
          </div>
          <div className="chips" style={{ marginBottom: 14 }}>
            {(["upi", "card", "cash"] as PaymentMethod[]).map((m) => (
              <button key={m} className={`chip ${method === m ? "active" : ""}`} onClick={() => setMethod(m)}>
                {m.toUpperCase()}
              </button>
            ))}
          </div>

          <button className="btn primary" onClick={props.onBook} disabled={busy}>
            {busy ? "Booking…" : `Book ${TIER_META[tier].label} · ${rupees(quote.prices[tier])}`}
          </button>
        </>
      )}
    </>
  );
}

function FindingView({ onCancel }: { onCancel: () => void }) {
  return (
    <div className="finding">
      <div className="radar">
        <div className="core" />
      </div>
      <h3>Finding your driver…</h3>
      <p className="sub">Matching you with the nearest available driver</p>
      <button className="btn danger" onClick={onCancel}>
        Cancel request
      </button>
    </div>
  );
}

function ActiveView({
  ride,
  status,
  otp,
  onCancel,
}: {
  ride: RideView;
  status: RideStatus;
  otp: string | null;
  onCancel?: () => void;
}) {
  const rank = STATUS_RANK[status] ?? 0;
  const steps = [
    { label: "Driver assigned", at: 1 },
    { label: "On the way to you", at: 2 },
    { label: "Arrived at pickup", at: 3 },
    { label: "On trip", at: 4 },
  ];
  const heading =
    status === "IN_PROGRESS"
      ? "On your way"
      : status === "ARRIVED"
        ? "Your driver has arrived"
        : status === "DRIVER_ARRIVING"
          ? "Driver is on the way"
          : "Driver assigned";

  return (
    <>
      <h3>{heading}</h3>
      {ride.driver && (
        <div className="dcard">
          <div className="avatar">{ride.driver.name.charAt(0)}</div>
          <div className="info">
            <div className="n">{ride.driver.name}</div>
            <div className="v">{ride.driver.vehicle_model}</div>
            <span className="plate">{ride.driver.plate}</span>
          </div>
          <div className="rating">★ {ride.driver.rating.toFixed(1)}</div>
        </div>
      )}

      {otp && rank < 4 && (
        <div className="otp-box">
          <div className="lbl">Share this OTP with your driver</div>
          <div className="digits">{otp}</div>
        </div>
      )}

      <div className="timeline">
        {steps.map((s) => (
          <div key={s.at} className={`tl-step ${rank > s.at ? "done" : rank === s.at ? "active" : ""}`}>
            <span className="bullet">{rank > s.at ? "✓" : ""}</span>
            {s.label}
          </div>
        ))}
      </div>

      {onCancel && (
        <button className="btn danger" onClick={onCancel}>
          Cancel ride
        </button>
      )}
    </>
  );
}

function CompletedView({
  ride,
  fare,
  paymentStatus,
  busy,
  onPay,
  onDone,
  onHistory,
}: {
  ride: RideView;
  fare: StatusChangedData["fare"] | null;
  paymentStatus: string | null;
  busy: boolean;
  onPay: () => void;
  onDone: () => void;
  onHistory: () => void;
}) {
  const total = fare?.total ?? ride.fare_total ?? 0;
  const paid = paymentStatus === "SUCCEEDED";
  return (
    <>
      <h3>{paid ? "Trip complete" : "You've arrived"}</h3>
      <p className="sub">{paid ? "Thanks for riding with GoRide" : "Here's your fare breakdown"}</p>

      <div className="fare">
        {fare ? (
          <>
            <div className="line">
              <span>Base fare</span>
              <span>{rupees(fare.base)}</span>
            </div>
            <div className="line">
              <span>Distance</span>
              <span>{rupees(fare.distance_component)}</span>
            </div>
            <div className="line">
              <span>Time</span>
              <span>{rupees(fare.time_component)}</span>
            </div>
            {fare.surge_x100 > 100 && (
              <div className="line">
                <span>Surge</span>
                <span>{surgeLabel(fare.surge_x100 / 100)}</span>
              </div>
            )}
          </>
        ) : (
          <div className="line">
            <span>Total fare</span>
            <span />
          </div>
        )}
        <div className="line total">
          <span>Total</span>
          <span>{rupees(total)}</span>
        </div>
      </div>

      {paid ? (
        <>
          <div className="row" style={{ justifyContent: "center", marginBottom: 12 }}>
            <span className="badge green">Paid via {ride.payment_method?.toUpperCase()}</span>
          </div>
          <button className="btn dark" onClick={onHistory} style={{ marginBottom: 8 }}>
            View receipt in history
          </button>
          <button className="btn primary" onClick={onDone}>
            Book another ride
          </button>
        </>
      ) : (
        <button className="btn go" onClick={onPay} disabled={busy}>
          {busy || paymentStatus === "PROCESSING"
            ? "Processing payment…"
            : paymentStatus === "FAILED"
              ? `Retry payment · ${rupees(total)}`
              : `Pay ${rupees(total)} · ${ride.payment_method?.toUpperCase()}`}
        </button>
      )}
    </>
  );
}

function TerminalView({ status, onDone }: { status: RideStatus; onDone: () => void }) {
  const msg =
    status === "EXPIRED"
      ? "No drivers were available"
      : status === "CANCELLED_BY_DRIVER"
        ? "Driver cancelled the ride"
        : "Ride cancelled";
  return (
    <>
      <h3>{msg}</h3>
      <p className="sub">
        {status === "EXPIRED"
          ? "We couldn't find a driver nearby. Try enabling Demo mode to add drivers, or book again."
          : "Your ride was cancelled."}
      </p>
      <button className="btn primary" onClick={onDone}>
        Book another ride
      </button>
    </>
  );
}

function HistoryView({ items, onBack }: { items: HistoryItem[]; onBack: () => void }) {
  return (
    <>
      <div className="row" style={{ justifyContent: "space-between" }}>
        <h3>Your rides</h3>
        <button className="link-btn" onClick={onBack}>
          Back
        </button>
      </div>
      {items.length === 0 && <p className="sub">No rides yet.</p>}
      {items.map((it) => (
        <div key={it.ride_id} className="hist-item">
          <div className="h-tier">{TIER_META[it.tier].icon}</div>
          <div className="h-main">
            <div>
              {it.driver ? it.driver.name : "—"}{" "}
              <span
                className={`badge ${it.receipt ? "green" : it.status.startsWith("CANCELLED") || it.status === "EXPIRED" ? "red" : "gray"}`}
              >
                {it.receipt ? "Paid" : it.status.replace(/_/g, " ").toLowerCase()}
              </span>
            </div>
            <div className="h-status">{new Date(it.created_at).toLocaleString()}</div>
          </div>
          <div className="h-fare">{it.fare_total != null ? rupees(it.fare_total) : "—"}</div>
        </div>
      ))}
    </>
  );
}
