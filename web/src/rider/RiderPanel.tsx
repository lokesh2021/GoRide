import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Api } from "../api/client";
import type {
  DriverLocationData,
  FareBreakdown,
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
import { PLACES, RIDERS, placeName } from "../config/personas";
import { bearing, buildRoute, formatDuration, formatKm, type LatLng } from "../lib/geo";
import { rupees, surgeLabel } from "../lib/money";
import { fetchRoad } from "../lib/routing";
import { MapView, type BotMarker } from "../map/MapView";
import { openStream } from "../sse/stream";
import { ConfirmDialog } from "../ui/dialog";
import { Spinner } from "../ui/spinner";
import { useToast } from "../ui/toast";
import { Receipt, receiptFromBreakdown, type ReceiptModel } from "./Receipt";

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

const MAX_RETRIES = 3;

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
  const [busy, setBusy] = useState<string | null>(null);
  const [booting, setBooting] = useState(true);

  const [ride, setRide] = useState<RideView | null>(null);
  const [status, setStatus] = useState<RideStatus | null>(null);
  const [otp, setOtp] = useState<string | null>(null);
  const [fare, setFare] = useState<FareBreakdown | null>(null);
  const [paymentStatus, setPaymentStatus] = useState<string | null>(null);
  const [retryCount, setRetryCount] = useState(0);
  const [payOpen, setPayOpen] = useState(false);
  const [justCompleted, setJustCompleted] = useState(false);
  const [driverPos, setDriverPos] = useState<LatLng | null>(null);
  const [driverBrg, setDriverBrg] = useState(0);
  const [reconnecting, setReconnecting] = useState(false);
  const [confirmCancel, setConfirmCancel] = useState(false);
  const [cancelReason, setCancelReason] = useState("changed my mind");
  const [roadPath, setRoadPath] = useState<LatLng[] | null>(null);

  const [showHistory, setShowHistory] = useState(false);
  const [history, setHistory] = useState<HistoryItem[]>([]);
  const [historyLoading, setHistoryLoading] = useState(false);

  const prevDriverPos = useRef<LatLng | null>(null);

  useEffect(() => onPickupChange(pickup), [pickup, onPickupChange]);

  // Reset everything when the persona switches, then rehydrate any ride that
  // is still live server-side (page refresh / tab loss must not orphan an
  // in-flight ride — the backend has it; the client re-attaches).
  useEffect(() => {
    setBooting(true);
    setRide(null);
    setStatus(null);
    setQuote(null);
    setOtp(null);
    setFare(null);
    setPaymentStatus(null);
    setRetryCount(0);
    setPayOpen(false);
    setJustCompleted(false);
    setDriverPos(null);
    setShowHistory(false);

    let stale = false;
    (async () => {
      try {
        const st = await api.riderState(persona.id);
        if (stale || !st.active_ride) return;
        setRide(st.active_ride);
        setStatus(st.active_ride.status);
        const saved = localStorage.getItem(`goride:otp:${persona.id}`);
        if (saved) {
          const [savedRide, savedOtp] = saved.split(":");
          if (savedRide === st.active_ride.id) setOtp(savedOtp);
          else localStorage.removeItem(`goride:otp:${persona.id}`);
        }
      } catch {
        // State lookup is best-effort on load; booking still works without it.
      } finally {
        if (!stale) setBooting(false);
      }
    })();
    return () => {
      stale = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [persona.id]);

  // ---- SSE: subscribe to the ride channel while a ride is live ----
  useEffect(() => {
    if (!ride) return;
    const handle = openStream(`/v1/events?ride_id=${ride.id}`, persona.token, {
      onStatus: (s) => setReconnecting(s === "reconnecting"),
      onEvent: (env: SSEEnvelope) => {
        switch (env.type) {
          case "ride.status_changed": {
            const d = env.data as StatusChangedData;
            setStatus(d.status);
            if (d.driver) setRide((r) => (r ? { ...r, driver: d.driver, driver_id: r.driver_id ?? "assigned" } : r));
            if (d.fare) setFare(d.fare as FareBreakdown);
            if (d.status === "COMPLETED") setJustCompleted(true);
            break;
          }
          case "ride.otp":
            setOtp((env.data as OtpData).otp);
            localStorage.setItem(`goride:otp:${persona.id}`, `${ride.id}:${(env.data as OtpData).otp}`);
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
            setRetryCount(d.retry_count);
            break;
          }
        }
      },
    });
    return () => {
      handle.close();
      setReconnecting(false);
    };
  }, [ride?.id, persona.token]); // eslint-disable-line react-hooks/exhaustive-deps

  // Auto-hide the "trip completed" banner after a few seconds.
  useEffect(() => {
    if (!justCompleted) return;
    const t = setTimeout(() => setJustCompleted(false), 5000);
    return () => clearTimeout(t);
  }, [justCompleted]);

  // On payment success, briefly show the check then drop the sheet, revealing
  // the paid receipt underneath.
  useEffect(() => {
    if (payOpen && paymentStatus === "SUCCEEDED") {
      const t = setTimeout(() => setPayOpen(false), 1200);
      return () => clearTimeout(t);
    }
  }, [payOpen, paymentStatus]);

  // Belt-and-braces: while the payment sheet is pending, poll in case an SSE
  // payment.updated was missed (dropped stream). GET /v1/rides/{id} confirms
  // the ride is still reachable; the ride view carries no payment status, so
  // success is detected via the history receipt (written on SUCCEEDED). Stops
  // on resolution or after 30s.
  useEffect(() => {
    if (!payOpen || !ride) return;
    if (paymentStatus === "SUCCEEDED" || (paymentStatus === "FAILED" && retryCount >= MAX_RETRIES)) return;
    let stop = false;
    const started = Date.now();
    const iv = setInterval(async () => {
      if (stop || Date.now() - started > 30000) {
        clearInterval(iv);
        return;
      }
      try {
        await api.getRide(ride.id); // liveness re-check
        const h = await api.history(persona.id);
        const item = h.rides.find((x) => x.ride_id === ride.id);
        if (item?.receipt) {
          setPaymentStatus("SUCCEEDED");
          clearInterval(iv);
        }
      } catch {
        // ignore — SSE may still resolve it
      }
    }, 3000);
    return () => {
      stop = true;
      clearInterval(iv);
    };
  }, [payOpen, paymentStatus, retryCount, ride?.id, persona.id]); // eslint-disable-line react-hooks/exhaustive-deps

  // ---- OSRM road polyline (pickup → drop) once a quote or ride exists ----
  useEffect(() => {
    const showFor = quote != null || ride != null;
    if (!pickup || !drop || !showFor) {
      setRoadPath(null);
      return;
    }
    let cancelled = false;
    setRoadPath(buildRoute(pickup, drop)); // immediate local fallback
    fetchRoad(pickup, drop).then((r) => {
      if (!cancelled && r) setRoadPath(r.path);
    });
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pickup, drop, quote?.quote_id, ride?.id]);

  const getFares = async () => {
    if (!pickup || !drop) {
      toast.info("Set both pickup and destination");
      return;
    }
    setBusy("fares");
    try {
      const q = await api.quote({ pickup: { lat: pickup[0], lng: pickup[1] }, drop: { lat: drop[0], lng: drop[1] } });
      setQuote(q);
    } catch (e) {
      toast.error(e, "Could not fetch fares");
    } finally {
      setBusy(null);
    }
  };

  const book = async () => {
    if (!quote) return;
    setBusy("book");
    try {
      const r = await api.createRide({ quote_id: quote.quote_id, tier, payment_method: method });
      setRide(r);
      setStatus(r.status);
      setQuote(null);
    } catch (e) {
      toast.error(e, "Booking failed");
    } finally {
      setBusy(null);
    }
  };

  const doCancel = async () => {
    if (!ride) return;
    setBusy("cancel");
    try {
      await api.cancelRide(ride.id, cancelReason.trim() || "changed my mind");
      toast.info("Ride cancelled");
      setConfirmCancel(false);
      resetToPlan();
    } catch (e) {
      toast.error(e, "Cancel failed");
    } finally {
      setBusy(null);
    }
  };

  const pay = async () => {
    if (!ride) return;
    setPayOpen(true);
    setPaymentStatus("PROCESSING");
    try {
      const p = await api.pay(ride.id);
      setPaymentStatus(p.status);
      setRetryCount(p.retry_count);
    } catch (e) {
      toast.error(e, "Payment failed");
      setPaymentStatus("FAILED");
    }
  };

  const loadHistory = async () => {
    setShowHistory(true);
    setHistoryLoading(true);
    try {
      const h = await api.history(persona.id);
      setHistory(h.rides);
    } catch (e) {
      toast.error(e, "Could not load history");
    } finally {
      setHistoryLoading(false);
    }
  };

  const reissueOtp = async () => {
    if (!ride) return;
    setBusy("otp");
    try {
      const res = await api.regenerateOtp(ride.id);
      setOtp(res.otp);
      localStorage.setItem(`goride:otp:${persona.id}`, `${ride.id}:${res.otp}`);
    } catch (e) {
      toast.error(e, "Could not fetch OTP");
    } finally {
      setBusy(null);
    }
  };

  const resetToPlan = useCallback(() => {
    setRide(null);
    setStatus(null);
    setOtp(null);
    setFare(null);
    setPaymentStatus(null);
    setRetryCount(0);
    setPayOpen(false);
    setJustCompleted(false);
    setDriverPos(null);
    prevDriverPos.current = null;
    localStorage.removeItem(`goride:otp:${persona.id}`);
  }, [persona.id]);

  // ---- derived map props ----
  const fallbackRoute = useMemo<LatLng[] | null>(
    () => (pickup && drop ? buildRoute(pickup, drop) : null),
    [pickup, drop],
  );
  const showRoute = quote != null || (ride != null && status != null && STATUS_RANK[status] >= 1);
  const mapRoute = showRoute ? (roadPath ?? fallbackRoute) : null;

  const fitPoints = useMemo<LatLng[]>(() => {
    const pts: LatLng[] = [];
    if (pickup) pts.push(pickup);
    if (drop) pts.push(drop);
    if (driverPos) pts.push(driverPos);
    return pts;
  }, [pickup, drop, driverPos]);

  const cancelTitle = status === "MATCHING" || status == null ? "Cancel request?" : "Cancel ride?";

  return (
    <div className="panel">
      <div className="panel-map">
        <MapView
          center={pickup ?? [12.9716, 77.5946]}
          pickup={pickup}
          drop={drop}
          route={mapRoute}
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

      {reconnecting && (
        <div className="sse-pill" role="status">
          <span className="spinner" aria-hidden="true" />
          Reconnecting…
        </div>
      )}

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
        {booting ? (
          <PanelShimmer />
        ) : showHistory ? (
          <HistoryView items={history} loading={historyLoading} onBack={() => setShowHistory(false)} />
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
            justCompleted={justCompleted}
            onPay={pay}
            onDone={resetToPlan}
            onHistory={loadHistory}
          />
        ) : status && STATUS_RANK[status] >= 1 ? (
          <ActiveView
            ride={ride}
            status={status}
            otp={otp}
            onCancel={canCancel(status) ? () => setConfirmCancel(true) : undefined}
            onReissueOtp={reissueOtp}
            busy={busy}
          />
        ) : (
          <FindingView onCancel={() => setConfirmCancel(true)} />
        )}
      </div>

      {payOpen && ride && (
        <PaymentSheet
          method={ride.payment_method ?? "upi"}
          amount={fare?.total ?? ride.fare_total ?? 0}
          status={paymentStatus}
          retryCount={retryCount}
          onRetry={pay}
          onClose={() => setPayOpen(false)}
        />
      )}

      {confirmCancel && (
        <ConfirmDialog
          title={cancelTitle}
          message="Let us know why (optional)."
          confirmLabel="Cancel ride"
          cancelLabel="Keep ride"
          tone="danger"
          busy={busy === "cancel"}
          onConfirm={doCancel}
          onCancel={() => setConfirmCancel(false)}
        >
          <input
            className="input dialog-input"
            value={cancelReason}
            onChange={(e) => setCancelReason(e.target.value)}
            placeholder="Reason (optional)"
            aria-label="Cancellation reason"
          />
        </ConfirmDialog>
      )}
    </div>
  );
}

// ---- sub-views ----

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
  busy: string | null;
  onGetFares: () => void;
  onBook: () => void;
  onHistory: () => void;
}) {
  const { pickup, drop, picking, setPicking, setPickup, setDrop, quote, tier, setTier, method, setMethod, busy } = props;

  const label = (p: LatLng | null) => (p ? placeName(p[0], p[1]) : "Tap map or pick a place");

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
            <div className="t">{label(pickup)}</div>
          </div>
          <button className="btn ghost" onClick={() => setPicking(picking === "pickup" ? null : "pickup")}>
            {picking === "pickup" ? "Tap map…" : "Set"}
          </button>
        </div>
        <div className="leg-row" style={{ outline: picking === "drop" ? "2px solid var(--accent)" : "none" }}>
          <span className="marker drop" />
          <div className="txt">
            <div className="c">Destination</div>
            <div className="t">{label(drop)}</div>
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
        <button className="btn primary" onClick={props.onGetFares} disabled={busy !== null || !pickup || !drop}>
          {busy === "fares" ? <Spinner label="Getting fares…" /> : "Get fares"}
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

          <button className="btn primary" onClick={props.onBook} disabled={busy !== null}>
            {busy === "book" ? <Spinner label="Booking…" /> : `Book ${TIER_META[tier].label} · ${rupees(quote.prices[tier])}`}
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
  onReissueOtp,
  busy,
}: {
  ride: RideView;
  status: RideStatus;
  otp: string | null;
  onCancel?: () => void;
  onReissueOtp: () => void;
  busy: string | null;
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
      {!otp && rank >= 1 && rank < 4 && (
        <div className="otp-box">
          <div className="lbl">OTP not on this device (opened in a new tab?)</div>
          <button className="btn dark" disabled={busy !== null} onClick={onReissueOtp}>
            {busy === "otp" ? <Spinner label="Fetching…" /> : "Show OTP"}
          </button>
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

// Build a receipt model from the live completion event (fare) + ride coords.
function completedReceipt(ride: RideView, fare: FareBreakdown | null, paid: boolean): ReceiptModel {
  return {
    rideId: ride.id,
    dateISO: fare?.ended_at ?? ride.updated_at ?? ride.created_at,
    pickup: [ride.pickup_lat, ride.pickup_lng],
    drop: [ride.drop_lat, ride.drop_lng],
    base: fare?.base ?? 0,
    distanceComponent: fare?.distance_component ?? 0,
    timeComponent: fare?.time_component ?? 0,
    surgeX100: fare?.surge_x100 ?? 100,
    total: fare?.total ?? ride.fare_total ?? 0,
    distanceM: fare?.distance_m,
    durationS: fare?.duration_s,
    startedAt: fare?.started_at,
    endedAt: fare?.ended_at,
    method: ride.payment_method ?? undefined,
    paid,
  };
}

function CompletedView({
  ride,
  fare,
  paymentStatus,
  justCompleted,
  onPay,
  onDone,
  onHistory,
}: {
  ride: RideView;
  fare: FareBreakdown | null;
  paymentStatus: string | null;
  justCompleted: boolean;
  onPay: () => void;
  onDone: () => void;
  onHistory: () => void;
}) {
  const total = fare?.total ?? ride.fare_total ?? 0;
  const paid = paymentStatus === "SUCCEEDED";
  const model = completedReceipt(ride, fare, paid);

  return (
    <>
      {justCompleted && !paid && (
        <div className="success-banner" role="status">
          ✅ Trip completed — fare {rupees(total)}
        </div>
      )}

      <Receipt model={model} />

      {paid ? (
        <>
          <button className="btn dark" onClick={onHistory} style={{ marginTop: 10, marginBottom: 8 }}>
            View in ride history
          </button>
          <button className="btn primary" onClick={onDone}>
            Book another ride
          </button>
        </>
      ) : (
        <button className="btn go" onClick={onPay} style={{ marginTop: 10 }}>
          Pay {rupees(total)} · {ride.payment_method?.toUpperCase()}
        </button>
      )}
    </>
  );
}

function PaymentSheet({
  method,
  amount,
  status,
  retryCount,
  onRetry,
  onClose,
}: {
  method: PaymentMethod;
  amount: number;
  status: string | null;
  retryCount: number;
  onRetry: () => void;
  onClose: () => void;
}) {
  const succeeded = status === "SUCCEEDED";
  const failed = status === "FAILED";
  const terminal = failed && retryCount >= MAX_RETRIES;

  const pendingCopy =
    method === "upi"
      ? "Approve the request in your UPI app…"
      : method === "card"
        ? "Processing card…"
        : `Collect ${rupees(amount)} in cash`;

  return (
    <div
      className="dialog-backdrop"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget && (succeeded || terminal)) onClose();
      }}
    >
      <div className="dialog pay-sheet" role="dialog" aria-modal="true" aria-label="Payment">
        <div className="pay-head">
          <span className="pay-method">{method.toUpperCase()}</span>
          <span className="pay-amount">{rupees(amount)}</span>
        </div>

        {succeeded ? (
          <div className="pay-state ok">
            <div className="check-anim">✓</div>
            <div className="pay-msg">Payment received</div>
          </div>
        ) : failed ? (
          <div className="pay-state err">
            <div className="pay-x">✕</div>
            <div className="pay-msg">
              {terminal ? "Payment failed after 3 attempts" : "Payment failed"}
            </div>
            {terminal ? (
              <button className="btn dark" onClick={onClose}>
                Close
              </button>
            ) : (
              <button className="btn go" onClick={onRetry}>
                Retry payment
              </button>
            )}
          </div>
        ) : (
          <div className="pay-state pending">
            <span className="spinner big" aria-hidden="true" />
            <div className="pay-msg">{pendingCopy}</div>
            {method === "cash" && <div className="muted">Cash is confirmed through the same secure flow.</div>}
          </div>
        )}
      </div>
    </div>
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

function HistoryView({ items, loading, onBack }: { items: HistoryItem[]; loading: boolean; onBack: () => void }) {
  const [expanded, setExpanded] = useState<string | null>(null);

  return (
    <>
      <div className="row" style={{ justifyContent: "space-between" }}>
        <h3>Your rides</h3>
        <button className="link-btn" onClick={onBack}>
          Back
        </button>
      </div>

      {loading ? (
        <>
          {[0, 1, 2, 3].map((i) => (
            <div key={i} className="hist-item">
              <div className="sk sk-dot" />
              <div className="h-main">
                <div className="sk sk-line" style={{ width: "60%" }} />
                <div className="sk sk-line" style={{ width: "40%", marginTop: 6 }} />
              </div>
            </div>
          ))}
        </>
      ) : items.length === 0 ? (
        <p className="sub">No rides yet.</p>
      ) : (
        items.map((it) => {
          const paid = !!it.receipt;
          const open = expanded === it.ride_id;
          const model: ReceiptModel | null = it.receipt
            ? receiptFromBreakdown(
                it.ride_id,
                it.receipt.created_at ?? it.created_at,
                [it.pickup_lat, it.pickup_lng],
                [it.drop_lat, it.drop_lng],
                it.receipt.breakdown,
                it.receipt.total,
                true,
              )
            : null;
          return (
            <div key={it.ride_id} className={`hist-wrap ${open ? "open" : ""}`}>
              <button
                className="hist-item as-btn"
                onClick={() => model && setExpanded(open ? null : it.ride_id)}
                disabled={!model}
                aria-expanded={open}
              >
                <div className="h-tier">{TIER_META[it.tier].icon}</div>
                <div className="h-main">
                  <div>
                    {it.driver ? it.driver.name : "—"}{" "}
                    <span
                      className={`badge ${paid ? "green" : it.status.startsWith("CANCELLED") || it.status === "EXPIRED" ? "red" : "gray"}`}
                    >
                      {paid ? "Paid" : it.status.replace(/_/g, " ").toLowerCase()}
                    </span>
                  </div>
                  <div className="h-status">
                    {new Date(it.created_at).toLocaleString()}
                    {model && <span className="h-expand">{open ? " · hide receipt" : " · view receipt"}</span>}
                  </div>
                </div>
                <div className="h-fare">{it.fare_total != null ? rupees(it.fare_total) : "—"}</div>
              </button>
              {open && model && (
                <div className="hist-receipt">
                  <Receipt model={model} />
                </div>
              )}
            </div>
          );
        })
      )}
    </>
  );
}
