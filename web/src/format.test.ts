import { describe, expect, it } from "vitest";
import { basename, bytes, crumbs, eta, join, speed, summarise } from "./format";

describe("bytes", () => {
  const cases: [number, string][] = [
    [0, "0 B"],
    [-1, "0 B"],
    [NaN, "0 B"],
    [512, "512 B"],
    [1024, "1.0 KB"],
    [1536, "1.5 KB"],
    [10 * 1024, "10 KB"],
    [1024 ** 3, "1.0 GB"],
    [1024 ** 5, "1.0 PB"],
    [1024 ** 6, "1024 PB"], // clamped to the last unit rather than overflowing it
  ];
  it.each(cases)("formats %d", (n, want) => expect(bytes(n)).toBe(want));
});

describe("speed and eta", () => {
  it("shows a dash when there is nothing to show", () => {
    expect(speed(0)).toBe("—");
    expect(eta(0)).toBe("—");
    expect(eta(-5)).toBe("—");
    expect(eta(Infinity)).toBe("—");
  });
  it("formats rates and durations", () => {
    expect(speed(2048)).toBe("2.0 KB/s");
    expect(eta(45)).toBe("45s");
    expect(eta(90)).toBe("1m 30s");
    expect(eta(3700)).toBe("1h 1m");
  });
});

describe("paths", () => {
  it("joins without doubling separators", () => {
    expect(join("/", "a")).toBe("/a");
    expect(join("/a", "b")).toBe("/a/b");
    expect(join("/a/", "b")).toBe("/a/b");
  });
  it("takes the last segment", () => {
    expect(basename("/a/b/c.txt")).toBe("c.txt");
    expect(basename("/a/b/")).toBe("b");
    expect(basename("/")).toBe("/");
  });
  it("builds a breadcrumb trail rooted at /", () => {
    expect(crumbs("/")).toEqual([{ name: "/", path: "/" }]);
    expect(crumbs("/a/b")).toEqual([
      { name: "/", path: "/" },
      { name: "a", path: "/a" },
      { name: "b", path: "/a/b" },
    ]);
  });
});

describe("summarise", () => {
  const items = [
    { size: 100, is_dir: false },
    { size: 200, is_dir: false },
    { size: 0, is_dir: true },
  ];
  it("counts files and folders separately", () => {
    expect(summarise(items)).toEqual({ count: 3, files: 2, dirs: 1, bytes: 300 });
  });
  it("is empty for an empty selection", () => {
    expect(summarise([])).toEqual({ count: 0, files: 0, dirs: 0, bytes: 0 });
  });
  it("never folds an unknown directory size into the byte total", () => {
    expect(summarise([{ size: 999, is_dir: true }]).bytes).toBe(0);
  });
});
