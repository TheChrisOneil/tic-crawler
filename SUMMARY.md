## Run 20261029T185246Z
```json
{
  "by_status": {
    "failed": 4,
    "ok": 2
  },
  "mrf_bytes": 187793005192,
  "mrf_urls": 812,
  "payers": 6,
  "toc_bytes": 597051
}
```
- **bcbs-kansas** failed mrfs=0 bytes=0 toc_bytes=0 valid=None errors=None FetchError: error https://mrf.secure.bcbsks.com/api/filedownloadhttptrigger?name=table-of-contents&ext=json (403 https://mrf.secure.bcbsks.com/api/filedownloadhttptrigger?name=table-of-contents&ext=json)
- **bcbs-michigan-employer** ok mrfs=518 bytes=111364705935 toc_bytes=292253 valid=False errors=10 
  - `reporting_structure[0].reporting_plans[0].plan_id_type: 'EIN' is not one of ['ein', 'hios']`
  - `reporting_structure[0].reporting_plans[0].plan_market_type: 'Group' is not one of ['group', 'individual']`
  - `reporting_structure[1].reporting_plans[0].plan_id_type: 'EIN' is not one of ['ein', 'hios']`
- **bcbs-michigan-group** ok mrfs=294 bytes=76428299257 toc_bytes=304798 valid=False errors=43 
  - `reporting_structure[0].reporting_plans[0].plan_id_type: 'HIOS' is not one of ['ein', 'hios']`
  - `reporting_structure[0].reporting_plans[0].plan_market_type: 'Group' is not one of ['group', 'individual']`
  - `reporting_structure[0].reporting_plans[1].plan_id_type: 'HIOS' is not one of ['ein', 'hios']`
- **bcbs-mississippi** failed mrfs=0 bytes=0 toc_bytes=0 valid=None errors=None FetchError: error https://tcr.bcbsms.com/Table_of_Contents/Local_TOC.json (robots_disallowed https://tcr.bcbsms.com/Table_of_Contents/Local_TOC.json)
- **bcbs-mississippi-state-employees** failed mrfs=0 bytes=0 toc_bytes=0 valid=None errors=None FetchError: error https://tcr.bcbsms.com/Table_of_Contents/State_TOC.json (robots_disallowed https://tcr.bcbsms.com/Table_of_Contents/State_TOC.json)
- **centene-ambetter** failed mrfs=0 bytes=0 toc_bytes=0 valid=None errors=None RobotsDisallowed: https://www.centene.com/price-transparency-files.html

### out/manifests/bcbs-michigan-employer/2026-10-29.csv: 518 rows
- http_status: {'200': 514, '403': 4}
- check_method: {'HEAD': 514, 'RANGE_GET': 4}

### out/manifests/bcbs-michigan-group/2026-10-29.csv: 294 rows
- http_status: {'': 1, '200': 293}
- check_method: {'': 1, 'HEAD': 293}
