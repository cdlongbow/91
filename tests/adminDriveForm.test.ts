import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const drivesPageSource = readFileSync(
  new URL("../src/admin/DrivesPage.tsx", import.meta.url),
  "utf8"
);

test("spider91 drive form does not expose advanced crawler credentials", () => {
  assert.doesNotMatch(drivesPageSource, /target_new/);
  assert.doesNotMatch(drivesPageSource, /crawl_hour/);
  assert.doesNotMatch(drivesPageSource, /python_path/);
  assert.doesNotMatch(drivesPageSource, /script_path/);
});

test("spider91 upload target uses explicit local-save option instead of auto target", () => {
  assert.match(drivesPageSource, /本地保存，不上传/);
  assert.doesNotMatch(drivesPageSource, /自动：唯一/);
  assert.doesNotMatch(drivesPageSource, /自动模式/);
});

test("onedrive drive form only exposes required default-app fields", () => {
  assert.match(
    drivesPageSource,
    /form\.kind !== "spider91" && form\.kind !== "onedrive"/
  );

  const match =
    /function credentialFields[\s\S]*?case "onedrive":\s*return \[([\s\S]*?)\];\s*case "spider91":/.exec(
      drivesPageSource
    );
  assert.ok(match, "onedrive credential field block should be present");
  const fields = match[1];

  assert.match(fields, /key: "refresh_token"/);
  assert.doesNotMatch(fields, /key: "access_token"/);
  assert.doesNotMatch(fields, /key: "api_url_address"/);
  assert.doesNotMatch(fields, /key: "region"/);
  assert.doesNotMatch(fields, /key: "is_sharepoint"/);
  assert.doesNotMatch(fields, /key: "site_id"/);
});
