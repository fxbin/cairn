/** @type {import('tailwindcss').Config} */
export default {
  content: ["./index.html", "./src/**/*.{js,ts,jsx,tsx}"],
  theme: {
    extend: {
      colors: {
        bg: "var(--c-bg)",
        panel: "var(--c-bg-panel)",
        border: "var(--c-border)",
        accent: "var(--c-accent)",
      },
    },
  },
  plugins: [],
};
