// A neutral mark for the Microsoft Teams channel (fork addition). It is an
// original glyph — a chat tile with a "T" — not the Microsoft Teams logo,
// which is a Microsoft trademark and not licensed for reuse here.
export function TeamsMark({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 24 24" aria-hidden="true" className={className} fill="currentColor">
      <path d="M5 3h10a3 3 0 0 1 3 3v8a3 3 0 0 1-3 3H10l-4.2 3.5c-.5.4-1.3.1-1.3-.6V17A3 3 0 0 1 2 14V6a3 3 0 0 1 3-3zm1.5 4v1.8h3v6.2h2V8.8h3V7z" />
      <circle cx="19.5" cy="5.5" r="2.5" />
    </svg>
  );
}
