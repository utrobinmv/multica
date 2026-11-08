// The Matrix brand mark below is a hand-drawn approximation of the official
// Matrix logo (two overlaid squares in a graph) using only currentColor so it
// inherits the surrounding text color. Simple Icons does not carry a CC0
// Matrix glyph, so no copied path is used.
export function MatrixMark({ className }: { className?: string }) {
  return (
    <svg
      viewBox="0 0 24 24"
      aria-hidden="true"
      className={className}
      fill="currentColor"
    >
      <path d="M4.2 2.6A2.6 2.6 0 0 1 6.8 0h10.4A2.6 2.6 0 0 1 19.8 2.6v4.6a2.6 2.6 0 0 1-2.6 2.6H6.8A2.6 2.6 0 0 1 4.2 7.2V2.6Zm0 9.2A2.6 2.6 0 0 1 6.8 9.2h10.4a2.6 2.6 0 0 1 2.6 2.6v9.6a2.6 2.6 0 0 1-2.6 2.6H6.8a2.6 2.6 0 0 1-2.6-2.6V11.8Z" />
    </svg>
  );
}