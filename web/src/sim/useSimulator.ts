import { useEffect, useState } from "react";
import { simulator } from "./simulator";
import type { LatLng } from "../lib/geo";

// React binding: subscribes to the shared simulator and returns the current bot
// positions, re-rendering on every tick (~1/sec).
export function useSimBots(): { id: string; pos: LatLng }[] {
  const [bots, setBots] = useState(() => simulator.getBots());
  useEffect(() => {
    return simulator.subscribe(() => setBots(simulator.getBots()));
  }, []);
  return bots;
}
