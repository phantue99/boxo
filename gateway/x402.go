package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/ipfs/boxo/gateway/x402"
)

func checkX402(w http.ResponseWriter, r *http.Request) (string, int, error) {
	var (
		network           = "aioz-testnet"
		usdcAddress       = "0x4654Dccb5aFc9D4E0106fC3Af9ABf4c5cc784D0E"
		facilitatorClient = x402.NewFacilitatorClient(&x402.FacilitatorConfig{
			URL: "http://localhost:4022",
		})
	)

	paymentRequirements := &x402.PaymentRequirements{
		Scheme:            "exact",
		Network:           network,
		MaxAmountRequired: "0.01",
		Resource:          "http://localhost:8181/" + r.URL.Path,
		Description:       "File pin",
		MimeType:          "*/*",
		PayTo:             "0x7B363fe9f9fc9d75e3CD56719aC79eA12Cd92b26",
		MaxTimeoutSeconds: 60,
		Asset:             usdcAddress,
	}

	if err := paymentRequirements.SetUSDCInfo(true); err != nil {
		fmt.Println("failed to set USDC info: ", err)
		return "", http.StatusInternalServerError, err
	}

	jsonPaymentReq, _ := json.Marshal(paymentRequirements)

	configScript := fmt.Sprintf(`
		<script>
			window.x402 = {
				amount: %s,
				paymentRequirements: %s,
				testnet: true,
				currentUrl: "%s",
				cdpClientKey: "",
				appName: "",
				appLogo: "",
				sessionTokenEndpoint: "",
			};
			console.log("payment requirements initialized: ", window.x402)
		</script>
	`,
		paymentRequirements.MaxAmountRequired,
		string(jsonPaymentReq),
		r.URL.Path,
	)

	paymentHeader := r.Header.Get("X-PAYMENT")
	paymentPayload, err := x402.DecodePaymentPayloadFromBase64(paymentHeader)
	if err != nil {
		fmt.Println("failed to decode payment header: ", err)
		return configScript, http.StatusPaymentRequired, err
	}

	response, err := facilitatorClient.Verify(paymentPayload, paymentRequirements)
	if err != nil {
		fmt.Println("failed to verify payment: ", err)
		return configScript, http.StatusInternalServerError, err
	}

	if !response.IsValid {
		fmt.Println("invalid payment: ", response.InvalidReason)
		return configScript, http.StatusPaymentRequired, errors.New(*response.InvalidReason)
	}

	fmt.Println("payment verified, proceeding")

	settleResponse, err := facilitatorClient.Settle(paymentPayload, paymentRequirements)
	if err != nil {
		fmt.Println("failed to settle payment: ", err)
		return configScript, http.StatusInternalServerError, err
	}

	settleResponseHeader, err := settleResponse.EncodeToBase64String()
	if err != nil {
		fmt.Println("failed to encode settle response: ", err)
		return configScript, http.StatusInternalServerError, err
	}

	w.Header().Set("X-PAYMENT-RESPONSE", settleResponseHeader)

	return configScript, http.StatusOK, nil
}
