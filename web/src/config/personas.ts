// Demo personas — seed tokens/UUIDs from migrations/0009_seed_demo_data.up.sql.
// These are fixed demo credentials, safe to hardcode for a local demo.

import type { Tier } from "../api/types";

export interface RiderPersona {
  id: string;
  name: string;
  token: string;
}

export interface DriverPersona {
  id: string;
  name: string;
  token: string;
  tier: Tier;
  vehicleModel: string;
  plate: string;
  rating: number;
}

export const RIDERS: RiderPersona[] = [
  { id: "00000000-0000-0000-0000-000000000001", name: "Ananya Rao", token: "rider1-token" },
  { id: "00000000-0000-0000-0000-000000000002", name: "Karthik Iyer", token: "rider2-token" },
];

export const DRIVERS: DriverPersona[] = [
  { id: "00000000-0000-0000-0000-000000000011", name: "Suresh Kumar", token: "driver1-token", tier: "mini", vehicleModel: "Maruti Alto", plate: "KA-01-AB-1234", rating: 4.5 },
  { id: "00000000-0000-0000-0000-000000000012", name: "Manjunath Gowda", token: "driver2-token", tier: "mini", vehicleModel: "Hyundai Santro", plate: "KA-01-AB-2345", rating: 4.6 },
  { id: "00000000-0000-0000-0000-000000000013", name: "Ramesh Naik", token: "driver3-token", tier: "sedan", vehicleModel: "Honda City", plate: "KA-01-CD-3456", rating: 4.7 },
  { id: "00000000-0000-0000-0000-000000000014", name: "Prakash Shetty", token: "driver4-token", tier: "sedan", vehicleModel: "Toyota Etios", plate: "KA-01-CD-4567", rating: 4.8 },
  { id: "00000000-0000-0000-0000-000000000015", name: "Girish Reddy", token: "driver5-token", tier: "xl", vehicleModel: "Toyota Innova", plate: "KA-01-EF-5678", rating: 4.9 },
  { id: "00000000-0000-0000-0000-000000000016", name: "Vinod Achar", token: "driver6-token", tier: "xl", vehicleModel: "Maruti Ertiga", plate: "KA-01-EF-6789", rating: 4.5 },
];

export function driverById(id: string): DriverPersona | undefined {
  return DRIVERS.find((d) => d.id === id);
}

// City centre + a few well-known Bengaluru spots for quick pickup/drop presets.
export const BLR_CENTER: [number, number] = [12.9716, 77.5946];

export interface Place {
  name: string;
  lat: number;
  lng: number;
}

export const PLACES: Place[] = [
  { name: "MG Road", lat: 12.9756, lng: 77.6068 },
  { name: "Koramangala", lat: 12.9352, lng: 77.6245 },
  { name: "Indiranagar", lat: 12.9784, lng: 77.6408 },
  { name: "Jayanagar", lat: 12.925, lng: 77.5938 },
  { name: "HSR Layout", lat: 12.9116, lng: 77.6389 },
  { name: "Whitefield", lat: 12.9698, lng: 77.75 },
  { name: "Hebbal", lat: 13.0358, lng: 77.597 },
  { name: "Electronic City", lat: 12.8452, lng: 77.6602 },
];

// Loose city bounds the simulator keeps bots inside of.
export const CITY_BOUNDS = {
  minLat: 12.86,
  maxLat: 13.06,
  minLng: 77.53,
  maxLng: 77.74,
};

export const CITY = "BLR";
