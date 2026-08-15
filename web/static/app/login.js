// login.js — first-run password setup, or sign-in when already configured.
import { getJSON, send } from "./api.js";

const form = document.getElementById("auth-form");
const pw = document.getElementById("auth-pw");
const err = document.getElementById("auth-err");
const submit = document.getElementById("auth-submit");
const title = document.getElementById("auth-title");
const hint = document.getElementById("auth-hint");

let setup = false;

(async () => {
    try {
        const st = await getJSON("/api/ui/auth/status");
        if (st.authenticated) { location = "/"; return; }
        setup = !st.configured;
    } catch { /* treat as login */ }

    title.textContent = setup ? "Set a password" : "Sign in";
    submit.textContent = setup ? "Create password" : "Sign in";
    hint.hidden = !setup;
    pw.focus();
})();

form.addEventListener("submit", async (e) => {
    e.preventDefault();
    err.hidden = true;
    submit.disabled = true;
    try {
        await send("POST", setup ? "/api/ui/auth/setup" : "/api/ui/auth/login", { password: pw.value });
        location = "/";
    } catch (ex) {
        err.textContent = ex.message;
        err.hidden = false;
        submit.disabled = false;
        pw.select();
    }
});
