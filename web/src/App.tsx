import { useCallback, useEffect, useMemo, useState } from "react";
import { DRIVERS, RIDERS } from "./config/personas";
import type { LatLng } from "./lib/geo";
import { RiderPanel } from "./rider/RiderPanel";
import { DriverPanel } from "./driver/DriverPanel";
import { simulator } from "./sim/simulator";
import { useSimBots } from "./sim/useSimulator";
import { ToastProvider } from "./ui/toast";

const DEFAULT_BOTS = 5;

function useBackendHealth(): "ok" | "down" | "checking" {
  const [state, setState] = useState<"ok" | "down" | "checking">("checking");
  useEffect(() => {
    let alive = true;
    const check = async () => {
      try {
        // Any HTTP response (even 400/401) proves the /v1 proxy reaches the API.
        await fetch("/v1/events", { method: "GET" });
        if (alive) setState("ok");
      } catch {
        if (alive) setState("down");
      }
    };
    check();
    const iv = setInterval(check, 8000);
    return () => {
      alive = false;
      clearInterval(iv);
    };
  }, []);
  return state;
}

export default function App() {
  const [riderId, setRiderId] = useState(RIDERS[0].id);
  const [driverId, setDriverId] = useState(DRIVERS[0].id);
  const [demo, setDemo] = useState(false);
  const [botCount, setBotCount] = useState(DEFAULT_BOTS);
  const [lastRiderPickup, setLastRiderPickup] = useState<LatLng | null>(null);

  const health = useBackendHealth();
  const bots = useSimBots();

  const rider = useMemo(() => RIDERS.find((r) => r.id === riderId)!, [riderId]);
  const driver = useMemo(() => DRIVERS.find((d) => d.id === driverId)!, [driverId]);

  // Bots are the driver personas NOT currently controlled in the driver panel.
  const botPersonas = useMemo(
    () => DRIVERS.filter((d) => d.id !== driverId).slice(0, botCount),
    [driverId, botCount],
  );

  // Drive the shared simulator from demo state.
  useEffect(() => {
    if (demo) simulator.start(botPersonas);
    else simulator.stop();
    return () => simulator.stop();
  }, [demo, botPersonas]);

  const onPickupChange = useCallback((p: LatLng | null) => setLastRiderPickup(p), []);

  return (
    <ToastProvider>
      <div className="app">
        <header className="app-header">
          <div className="wordmark">
            <span className="dot" />
            <span>
              <span className="go">Go</span>Ride
            </span>
          </div>

          <div className="header-spacer" />

          <div className="header-controls">
            <span className={`pill ${health === "ok" ? "ok" : health === "down" ? "bad" : ""}`}>
              <span className="dot" />
              {health === "ok" ? "API connected" : health === "down" ? "API unreachable" : "Connecting…"}
            </span>

            {demo && (
              <span className="pill ok">
                <span className="dot" />
                {bots.length} bot drivers live
              </span>
            )}

            <label className="row" style={{ gap: 8, fontSize: 13, fontWeight: 600 }}>
              Bots
              <select
                className="select"
                value={botCount}
                onChange={(e) => setBotCount(Number(e.target.value))}
                style={{ padding: "6px 8px" }}
              >
                {[1, 2, 3, 4, 5].map((n) => (
                  <option key={n} value={n}>
                    {n}
                  </option>
                ))}
              </select>
            </label>

            <div className="row" style={{ gap: 8, fontSize: 13, fontWeight: 600 }}>
              Demo mode
              <button
                type="button"
                role="switch"
                aria-checked={demo}
                aria-label="Demo mode"
                className={`toggle ${demo ? "on" : ""}`}
                onClick={() => setDemo(!demo)}
              >
                <div className="knob" />
              </button>
            </div>
          </div>
        </header>

        <div className="stage">
          <RiderPanel persona={rider} onPersonaChange={setRiderId} onPickupChange={onPickupChange} bots={bots} />
          <DriverPanel
            persona={driver}
            onPersonaChange={setDriverId}
            lastRiderPickup={lastRiderPickup}
            bots={bots}
          />
        </div>
      </div>
    </ToastProvider>
  );
}
