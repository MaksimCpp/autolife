import assert from "node:assert/strict";
import test from "node:test";
import { isLocale, messages } from "./messages.ts";

test("supported locales have the same labels", () => {
  assert.ok(isLocale("ru"));
  assert.ok(isLocale("en"));
  assert.equal(isLocale("fr"), false);
  assert.deepEqual(Object.keys(messages.ru), Object.keys(messages.en));
});
