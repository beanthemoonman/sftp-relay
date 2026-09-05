import js from "@eslint/js";
import ts from "typescript-eslint";
import hooks from "eslint-plugin-react-hooks";

export default ts.config(
  js.configs.recommended,
  ts.configs.recommended,
  {
    files: ["src/**/*.{ts,tsx}"],
    plugins: { "react-hooks": hooks },
    rules: {
      ...hooks.configs.recommended.rules,
      // ponytail: the two hits are deliberate — syncing local form/browse state
      // to server data that arrives asynchronously. Refactoring to `key=` resets
      // is a bigger change than the rule is worth here.
      "react-hooks/set-state-in-effect": "off",
    },
  },
  {
    files: ["src/**/*.test.{ts,tsx}"],
    rules: { "@typescript-eslint/no-explicit-any": "off" },
  },
);
