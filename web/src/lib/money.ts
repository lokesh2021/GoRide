// Money helpers. The API speaks integer paise (INR); the UI shows rupees.

export function rupees(paise: number): string {
  // Fares are rounded to whole rupees server-side, but format defensively.
  const r = paise / 100;
  return `₹${r.toLocaleString("en-IN", { maximumFractionDigits: r % 1 === 0 ? 0 : 2 })}`;
}

export function surgeLabel(surge: number): string {
  return `${surge.toFixed(1)}×`;
}
