## Testing

Disposable stack preparation and focused tests passed. Three live browser scenarios verified persistent AJAX changes, concurrent feedback, rejection recovery, and retry; screenshots and response transcripts were captured, then the stack and transient fixtures were removed.

- Live validation: ✅ go - 3 of 3 scenarios driven live against the product

| Scenario | Result | Live | Evidence |
| --- | --- | --- | --- |
| On the disposable local product, exclude and restore HU: feedback appears without navigation, and both states survive full reloads. | ✅ pass | live | [roundtrip.json](https://vps.nevilleweb.sk/no-mistakes/01M6NB7HE9AM2PB1JWRS60EN63/roundtrip.json); [hu-excluded.png](https://vps.nevilleweb.sk/no-mistakes/01M6NB7HE9AM2PB1JWRS60EN63/hu-excluded.png) |
| Save HU and CZ concurrently on a synthetic photo: each checkbox locks while saving and retains independent market feedback. | ✅ pass | live | [concurrent.json](https://vps.nevilleweb.sk/no-mistakes/01M6NB7HE9AM2PB1JWRS60EN63/concurrent.json); [both-pending.png](https://vps.nevilleweb.sk/no-mistakes/01M6NB7HE9AM2PB1JWRS60EN63/both-pending.png); [hu-done-cz-pending.png](https://vps.nevilleweb.sk/no-mistakes/01M6NB7HE9AM2PB1JWRS60EN63/hu-done-cz-pending.png) |
| Submit an invalid security token locally: HTTP 403 restores the checkbox, preserves its error when another market saves, and permits a successful retry without navigation. | ✅ pass | live | [rejected.json](https://vps.nevilleweb.sk/no-mistakes/01M6NB7HE9AM2PB1JWRS60EN63/rejected.json); [rejected.png](https://vps.nevilleweb.sk/no-mistakes/01M6NB7HE9AM2PB1JWRS60EN63/rejected.png) |

- Evidence: [HU exclusion feedback](https://vps.nevilleweb.sk/no-mistakes/01M6NB7HE9AM2PB1JWRS60EN63/hu-excluded.png)
- Evidence: [Both markets saving](https://vps.nevilleweb.sk/no-mistakes/01M6NB7HE9AM2PB1JWRS60EN63/both-pending.png)
- Evidence: [HU completed while CZ remains pending](https://vps.nevilleweb.sk/no-mistakes/01M6NB7HE9AM2PB1JWRS60EN63/hu-done-cz-pending.png)
- Evidence: [Rejected request feedback](https://vps.nevilleweb.sk/no-mistakes/01M6NB7HE9AM2PB1JWRS60EN63/rejected.png)

<details>
<summary>Evidence: Exclusion and restoration persistence</summary>

Source: [Exclusion and restoration persistence](https://vps.nevilleweb.sk/no-mistakes/01M6NB7HE9AM2PB1JWRS60EN63/roundtrip.json)

```text
{"phase":"exclude","checked":false,"feedback":"Táto fotka sa neodošle do HU. Ak je kópia už v obchode, odstráňte ju v Shoptete. ERP ju nehľadá.","unchanged":true,"timeOrigin":1793865776789.6,"responses":[{"url":"http://127.0.0.1:32872/admin/products/019ef500-0000-7000-8000-00000000eeee/images/01a1aab9-fa2d-7213-8ebb-571c4b21e11f/shops/HU","status":200,"payload":{"ok":true,"included":false,"level":"warning","message":"Táto fotka sa neodošle do HU. Ak je kópia už v obchode, odstráňte ju v Shoptete. ERP ju nehľadá."}}]}
{"phase":"after real reload","HU":false,"CZ":false,"SK":true,"RO":true,"timeOrigin":1793865807979.8}
{"phase":"restore","checked":true,"feedback":"HU dostane túto fotku pri ďalšom odoslaní galérie.","unchanged":true,"timeOrigin":1793865807979.8}
{"phase":"restored after real reload","HU":true,"timeOrigin":1793865819002.4}
```
</details>
<details>
<summary>Evidence: Concurrent real AJAX responses and feedback</summary>

Source: [Concurrent real AJAX responses and feedback](https://vps.nevilleweb.sk/no-mistakes/01M6NB7HE9AM2PB1JWRS60EN63/concurrent.json)

```text
{"phase":"both pending","state":{"HU":{"checked":false,"disabled":true,"busy":"true","status":"Ukladá sa HU…","hidden":false},"CZ":{"checked":false,"disabled":true,"busy":"true","status":"Ukladá sa CZ…","hidden":false}}}
{"phase":"HU done CZ pending","state":{"HU":{"checked":false,"disabled":false,"busy":null,"status":"Táto fotka sa neodošle do HU. Ak je kópia už v obchode, odstráňte ju v Shoptete. ERP ju nehľadá.","hidden":false},"CZ":{"checked":false,"disabled":true,"busy":"true","status":"Ukladá sa CZ…","hidden":false}}}
{"phase":"both done","state":{"HU":{"checked":false,"disabled":false,"busy":null,"status":"Táto fotka sa neodošle do HU. Ak je kópia už v obchode, odstráňte ju v Shoptete. ERP ju nehľadá.","hidden":false},"CZ":{"checked":false,"disabled":false,"busy":null,"status":"Táto fotka sa neodošle do CZ. Ak je kópia už v obchode, odstráňte ju v Shoptete. ERP ju nehľadá.","hidden":false}},"responses":[{"market":"HU","status":200,"payload":{"ok":true,"included":false,"level":"warning","message":"Táto fotka sa neodošle do HU. Ak je kópia už v obchode, odstráňte ju v Shoptete. ERP ju nehľadá."}},{"market":"CZ","status":200,"payload":{"ok":true,"included":false,"level":"warning","message":"Táto fotka sa neodošle do CZ. Ak je kópia už v obchode, odstráňte ju v Shoptete. ERP ju nehľadá."}}],"unchanged":true}
```
</details>
<details>
<summary>Evidence: HTTP 403 recovery and successful retry</summary>

Source: [HTTP 403 recovery and successful retry](https://vps.nevilleweb.sk/no-mistakes/01M6NB7HE9AM2PB1JWRS60EN63/rejected.json)

```text
{"phase":"persisted concurrent saves","HU":false,"CZ":true,"timeOrigin":1793865776789.6}
{"phase":"rejected","checked":false,"disabled":false,"feedback":"Neplatný bezpečnostný token. Výber obchodu sa nepodarilo uložiť.","class":"small text-urgency-red-darker","responses":[{"url":"http://127.0.0.1:32872/admin/products/019ef500-0000-7000-8000-00000000eeee/images/01a1aab9-fa2d-7213-8ebb-571c4b21e11f/shops/HU","status":403,"payload":{"ok":false,"included":null,"level":"danger","message":"Neplatný bezpečnostný token. Výber obchodu sa nepodarilo uložiť."}}],"unchanged":true}
{"phase":"other save preserves error","huError":"Neplatný bezpečnostný token. Výber obchodu sa nepodarilo uložiť.","czFeedback":"Táto fotka sa neodošle do CZ. Ak je kópia už v obchode, odstráňte ju v Shoptete. ERP ju nehľadá."}
{"phase":"retry","checked":true,"feedback":"HU dostane túto fotku pri ďalšom odoslaní galérie.","unchanged":true,"responses":[{"url":"http://127.0.0.1:32872/admin/products/019ef500-0000-7000-8000-00000000eeee/images/01a1aab9-fa2d-7213-8ebb-571c4b21e11f/shops/HU","status":403,"payload":{"ok":false,"included":null,"level":"danger","message":"Neplatný bezpečnostný token. Výber obchodu sa nepodarilo uložiť."}},{"url":"http://127.0.0.1:32872/admin/products/019ef500-0000-7000-8000-00000000eeee/images/01a1aab9-fa2d-7213-8ebb-571c4b21e11f/shops/CZ","status":200,"payload":{"ok":true,"included":false,"level":"warning","message":"Táto fotka sa neodošle do CZ. Ak je kópia už v obchode, odstráňte ju v Shoptete. ERP ju nehľadá."}},{"url":"http://127.0.0.1:32872/admin/products/019ef500-0000-7000-8000-00000000eeee/images/01a1aab9-fa2d-7213-8ebb-571c4b21e11f/shops/HU","status":200,"payload":{"ok":true,"included":true,"level":"success","message":"HU dostane túto fotku pri ďalšom odoslaní galérie."}}]}
```
</details>
