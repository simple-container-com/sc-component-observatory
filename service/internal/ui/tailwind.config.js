/** @type {import('tailwindcss').Config} */
export default {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        bg: "#0b0d10",
        surface: "#14181d",
        line: "#232a32",
        fg: "#e6eaef",
        muted: "#9aa6b2",
        faint: "#6b7684",
        ok: "#3fb950",
        warn: "#d29922",
        danger: "#f85149",
        accent: "#58a6ff",
      },
    },
  },
  plugins: [],
};
